// This package provides utilities that underlie the specific commands.
package cmd

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"expvar"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"

	"google.golang.org/grpc/grpclog"

	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
)

// Because we don't know when this init will be called with respect to
// flag.Parse() and other flag definitions, we can't rely on the regular
// flag mechanism. But this one is fine.
func init() {
	for _, v := range os.Args {
		if v == "--version" || v == "-version" {
			fmt.Println(VersionString())
			os.Exit(0)
		}
	}
}

// mysqlLogger proxies blog.AuditLogger to provide a Print(...) method.
type mysqlLogger struct {
	blog.Logger
}

func (m mysqlLogger) Print(v ...interface{}) {
	m.AuditErrf("[mysql] %s", fmt.Sprint(v...))
}

// cfsslLogger provides two additional methods that are expected by CFSSL's
// logger but not supported by Boulder's Logger.
type cfsslLogger struct {
	blog.Logger
}

func (cl cfsslLogger) Crit(msg string) {
	cl.AuditErr(msg)
}

func (cl cfsslLogger) Emerg(msg string) {
	cl.AuditErr(msg)
}

type grpcLogger struct {
	blog.Logger
}

// V returns true if the verbosity level l is less than the verbosity we want to
// log at.
func (log grpcLogger) V(l int) bool {
	return l < 0
}

func (log grpcLogger) Fatal(args ...interface{}) {
	log.Error(args...)
	os.Exit(1)
}
func (log grpcLogger) Fatalf(format string, args ...interface{}) {
	log.Error(args...)
	os.Exit(1)
}
func (log grpcLogger) Fatalln(args ...interface{}) {
	log.Error(args...)
	os.Exit(1)
}

func (log grpcLogger) Error(args ...interface{}) {
	log.Logger.AuditErr(fmt.Sprintln(args...))
}
func (log grpcLogger) Errorf(format string, args ...interface{}) {
	output := fmt.Sprintf(format, args...)
	if output == `grpc: Server.processUnaryRPC failed to write status: connection error: desc = "transport is closing"` {
		return
	}
	log.Logger.AuditErr(output)
}
func (log grpcLogger) Errorln(args ...interface{}) {
	log.Logger.AuditErr(fmt.Sprintln(args...))
}

func (log grpcLogger) Warning(args ...interface{}) {
	log.Error(args...)
}
func (log grpcLogger) Warningf(format string, args ...interface{}) {
	log.Errorf(format, args...)
}
func (log grpcLogger) Warningln(args ...interface{}) {
	log.Errorln(args...)
}

// Don't log any INFO-level gRPC stuff. In practice this is all noise, like
// failed TXT lookups for service discovery (we only use A records).
func (log grpcLogger) Info(args ...interface{}) {
}
func (log grpcLogger) Infof(format string, args ...interface{}) {
}
func (log grpcLogger) Infoln(args ...interface{}) {
}

type promLogger struct {
	blog.Logger
}

func (log promLogger) Println(args ...interface{}) {
	log.AuditErr(fmt.Sprintln(args...))
}

// StatsAndLogging constructs a metrics.Scope and an AuditLogger based on its config
// parameters, and return them both. It also spawns off an HTTP server on the
// provided port to report the stats and provide pprof profiling handlers.
// Crashes if any setup fails.
// Also sets the constructed AuditLogger as the default logger, and configures
// the cfssl, mysql, and grpc packages to use our logger.
// This must be called before any gRPC code is called, because gRPC's SetLogger
// doesn't use any locking.
func StatsAndLogging(logConf SyslogConfig, addr string) (metrics.Scope, blog.Logger) {
	logger := NewLogger(logConf)
	scope := newScope(addr, logger)
	return scope, logger
}

func NewLogger(logConf SyslogConfig) blog.Logger {
	tag := path.Base(os.Args[0])
	syslogger, err := syslog.Dial(
		"",
		"",
		syslog.LOG_INFO, // default, not actually used
		tag)
	FailOnError(err, "Could not connect to Syslog")
	syslogLevel := int(syslog.LOG_INFO)
	if logConf.SyslogLevel != 0 {
		syslogLevel = logConf.SyslogLevel
	}
	logger, err := blog.New(syslogger, logConf.StdoutLevel, syslogLevel)
	FailOnError(err, "Could not connect to Syslog")

	_ = blog.Set(logger)
	// We set the cfssl logging level to Debug as it
	// won't actually call logging methods for any
	// level less than what is set. We will ignore
	// any logging we don't care about at the syslog
	// level, so this doesn't cause extraneous logging.
	cfsslLog.Level = cfsslLog.LevelDebug
	cfsslLog.SetLogger(cfsslLogger{logger})
	_ = mysql.SetLogger(mysqlLogger{logger})
	grpclog.SetLoggerV2(grpcLogger{logger})
	return logger
}

func newScope(addr string, logger blog.Logger) metrics.Scope {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector())
	registry.MustRegister(prometheus.NewProcessCollector(
		prometheus.ProcessCollectorOpts{}))

	mux := http.NewServeMux()
	// Register the available pprof handlers. These are all registered on
	// DefaultServeMux just by importing pprof, but since we eschew
	// DefaultServeMux, we need to explicitly register them on our own mux.
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	// These handlers are defined in runtime/pprof instead of net/http/pprof, and
	// have to be accessed through net/http/pprof's Handler func.
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	mux.Handle("/debug/vars", expvar.Handler())
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog: promLogger{logger},
	}))

	server := http.Server{
		Addr:    addr,
		Handler: mux,
	}
	go func() {
		err := server.ListenAndServe()
		if err != nil {
			log.Fatalf("unable to boot debug server on %s: %v", addr, err)
		}
	}()
	return metrics.NewPromScope(registry)
}

// Fail exits and prints an error message to stderr and the logger audit log.
func Fail(msg string) {
	logger := blog.Get()
	logger.AuditErr(msg)
	fmt.Fprintf(os.Stderr, msg)
	os.Exit(1)
}

// FailOnError exits and prints an error message, but only if we encountered
// a problem and err != nil
func FailOnError(err error, msg string) {
	if err != nil {
		msg := fmt.Sprintf("%s: %s", msg, err)
		Fail(msg)
	}
}

// LoadCert loads a PEM-formatted certificate from the provided path, returning
// it as a byte array, or an error if it couldn't be decoded.
func LoadCert(path string) (cert []byte, err error) {
	if path == "" {
		err = errors.New("Issuer certificate was not provided in config.")
		return
	}
	pemBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		err = errors.New("Invalid certificate value returned")
		return
	}

	cert = block.Bytes
	return
}

// ReadConfigFile takes a file path as an argument and attempts to
// unmarshal the content of the file into a struct containing a
// configuration of a boulder component.
func ReadConfigFile(filename string, out interface{}) error {
	configData, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(configData, out)
}

// VersionString produces a friendly Application version string.
func VersionString() string {
	name := path.Base(os.Args[0])
	return fmt.Sprintf("Versions: %s=(%s %s) Golang=(%s) BuildHost=(%s)", name, core.GetBuildID(), core.GetBuildTime(), runtime.Version(), core.GetBuildHost())
}

var signalToName = map[os.Signal]string{
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGHUP:  "SIGHUP",
}

// CatchSignals catches SIGTERM, SIGINT, SIGHUP and executes a callback
// method before exiting
func CatchSignals(logger blog.Logger, callback func()) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM)
	signal.Notify(sigChan, syscall.SIGINT)
	signal.Notify(sigChan, syscall.SIGHUP)

	sig := <-sigChan
	if logger != nil {
		logger.Infof("Caught %s", signalToName[sig])
	}

	if callback != nil {
		callback()
	}

	if logger != nil {
		logger.Info("Exiting")
	}
	os.Exit(0)
}

// FilterShutdownErrors returns the input error, with the exception of "use of
// closed network connection," on which it returns nil
// Per https://github.com/grpc/grpc-go/issues/1017, a gRPC server's `Serve()`
// will always return an error, even when GracefulStop() is called. We don't
// want to log graceful stops as errors, so we filter out the meaningless
// error we get in that situation.
func FilterShutdownErrors(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}
