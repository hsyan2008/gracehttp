package gracehttp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hsyan2008/go-logger"
)

const (
	GRACEFUL_ENVIRON_KEY    = "IS_GRACEFUL"
	GRACEFUL_ENVIRON_STRING = GRACEFUL_ENVIRON_KEY + "=1"
	GRACEFUL_LISTENER_FD    = 3
)

// HTTP server that supported graceful shutdown or restart
type Server struct {
	*http.Server

	//分开是为了解决getTCPListenerFd()里报错
	//panic: interface conversion: net.Listener is *tls.listener, not *net.TCPListener
	listener    net.Listener
	tlsListener net.Listener

	isGraceful   bool
	signalChan   chan os.Signal
	shutdownChan chan bool
}

func NewServer(addr string, handler http.Handler, readTimeout, writeTimeout time.Duration) *Server {
	isGraceful := false
	if os.Getenv(GRACEFUL_ENVIRON_KEY) != "" {
		isGraceful = true
	}

	return &Server{
		Server: &http.Server{
			Addr:    addr,
			Handler: handler,

			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
		},

		isGraceful:   isGraceful,
		signalChan:   make(chan os.Signal),
		shutdownChan: make(chan bool),
	}
}

func (srv *Server) InitListener() (net.Listener, error) {
	if srv.listener == nil {
		if srv.Addr == "" {
			return nil, fmt.Errorf("listen to nil addr")
		}
		ln, err := srv.getNetListener(srv.Addr)
		if err != nil {
			return nil, err
		}

		srv.listener = ln
	}

	return srv.listener, nil
}

func (srv *Server) ListenAndServe() error {
	_, err := srv.InitListener()
	if err != nil {
		return err
	}
	return srv.Serve()
}

func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	config := &tls.Config{}
	if srv.TLSConfig != nil {
		config = srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"h2", "http/1.1"}
	}

	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	_, err = srv.InitListener()
	if err != nil {
		return err
	}

	srv.tlsListener = tls.NewListener(srv.listener, config)
	return srv.Serve()
}

func (srv *Server) Serve() error {
	go srv.handleSignals()
	var err error
	if srv.tlsListener != nil {
		err = srv.Server.Serve(srv.tlsListener)
	} else {
		err = srv.Server.Serve(srv.listener)
	}

	logger.Info("waiting for connections closed.")
	<-srv.shutdownChan
	logger.Info("all connections closed.")

	return err
}

func (srv *Server) getNetListener(addr string) (net.Listener, error) {
	var ln net.Listener
	var err error

	if srv.isGraceful {
		file := os.NewFile(GRACEFUL_LISTENER_FD, "")
		ln, err = net.FileListener(file)
		if err != nil {
			err = fmt.Errorf("net.FileListener error: %v", err)
			return nil, err
		}
	} else {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			err = fmt.Errorf("net.Listen error: %v", err)
			return nil, err
		}
	}
	return ln, nil
}

func (srv *Server) handleSignals() {
	var sig os.Signal

	signal.Notify(
		srv.signalChan,

		syscall.SIGQUIT,
		syscall.SIGINT,

		syscall.SIGHUP,
		syscall.SIGTERM,
	)

	for {
		sig = <-srv.signalChan
		switch sig {
		case syscall.SIGQUIT, syscall.SIGINT:
			logger.Infof("received %s, graceful shutting down HTTP server.", sig)
			srv.shutdownHTTPServer()
		case syscall.SIGHUP, syscall.SIGTERM:
			logger.Infof("received %s, graceful restarting HTTP server.", sig)

			if pid, err := srv.startNewProcess(); err != nil {
				logger.Warnf("start new process failed: %v, continue serving.", err)
			} else {
				logger.Infof("start new process successed, the new pid is %d.", pid)
				srv.shutdownHTTPServer()
			}
		default:
		}
	}
}

func (srv *Server) shutdownHTTPServer() {
	if err := srv.Shutdown(context.Background()); err != nil {
		logger.Warnf("HTTP server shutdown error: %v", err)
	} else {
		logger.Info("HTTP server shutdown success.")
		srv.shutdownChan <- true
	}
}

// start new process to handle HTTP Connection
func (srv *Server) startNewProcess() (uintptr, error) {
	listenerFd, err := srv.getTCPListenerFd()
	if err != nil {
		return 0, fmt.Errorf("failed to get socket file descriptor: %v", err)
	}

	// set graceful restart env flag
	envs := []string{}
	for _, value := range os.Environ() {
		if value != GRACEFUL_ENVIRON_STRING {
			envs = append(envs, value)
		}
	}
	envs = append(envs, GRACEFUL_ENVIRON_STRING)

	execSpec := &syscall.ProcAttr{
		Env:   envs,
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd(), listenerFd},
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	//win不支持ForkExec
	// fork, err := syscall.ForkExec(os.Args[0], os.Args, execSpec)
	fork, _, err := syscall.StartProcess(os.Args[0], os.Args, execSpec)
	if err != nil {
		return 0, fmt.Errorf("failed to forkexec: %v", err)
	}

	return uintptr(fork), nil
}

func (srv *Server) getTCPListenerFd() (uintptr, error) {
	file, err := srv.listener.(*net.TCPListener).File()
	if err != nil {
		return 0, err
	}
	return file.Fd(), nil
}
