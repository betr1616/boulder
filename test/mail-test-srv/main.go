package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	blog "github.com/letsencrypt/boulder/log"
)

type mailSrv struct {
	closeFirst      uint
	closeDuration   time.Duration
	allReceivedMail []rcvdMail
	allMailMutex    sync.Mutex
	connNumber      uint
	connNumberMutex sync.RWMutex
}

type rcvdMail struct {
	From string
	To   string
	Mail string
}

func expectLine(buf *bufio.Reader, expected string) error {
	line, _, err := buf.ReadLine()
	if err != nil {
		return fmt.Errorf("readline: %v", err)
	}
	if string(line) != expected {
		return fmt.Errorf("Expected %s, got %s", expected, line)
	}
	return nil
}

var mailFromRegex = regexp.MustCompile("^MAIL FROM:<(.*)>\\s*BODY=8BITMIME\\s*$")
var rcptToRegex = regexp.MustCompile("^RCPT TO:<(.*)>\\s*$")
var dataRegex = regexp.MustCompile("^DATA\\s*$")
var smtpErr501 = []byte("501 syntax error in parameters or arguments \r\n")
var smtpOk250 = []byte("250 OK \r\n")

func (srv *mailSrv) handleConn(conn net.Conn) {
	defer conn.Close()
	srv.connNumberMutex.Lock()
	srv.connNumber++
	srv.connNumberMutex.Unlock()
	auditlogger := blog.Get()
	auditlogger.Info(fmt.Sprintf("mail-test-srv: Got connection from %s", conn.RemoteAddr()))

	readBuf := bufio.NewReader(conn)
	conn.Write([]byte("220 smtp.example.com ESMTP\r\n"))
	if err := expectLine(readBuf, "EHLO localhost"); err != nil {
		log.Printf("mail-test-srv: %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	conn.Write([]byte("250-PIPELINING\r\n"))
	conn.Write([]byte("250-AUTH PLAIN LOGIN\r\n"))
	conn.Write([]byte("250 8BITMIME\r\n"))
	if err := expectLine(readBuf, "AUTH PLAIN AGNlcnQtbWFzdGVyQGV4YW1wbGUuY29tAHBhc3N3b3Jk"); err != nil {
		log.Printf("mail-test-srv: %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	conn.Write([]byte("235 2.7.0 Authentication successful\r\n"))
	auditlogger.Info(fmt.Sprintf("mail-test-srv: Successful auth from %s", conn.RemoteAddr()))

	// necessary commands:
	// MAIL RCPT DATA QUIT

	var fromAddr string
	var toAddr []string

	clearState := func() {
		fromAddr = ""
		toAddr = nil
	}

	reader := bufio.NewScanner(readBuf)
	for reader.Scan() {
		line := reader.Text()
		cmdSplit := strings.SplitN(line, " ", 2)
		cmd := cmdSplit[0]
		switch cmd {
		case "QUIT":
			conn.Write([]byte("221 Bye \r\n"))
			break
		case "RSET":
			clearState()
			conn.Write(smtpOk250)
		case "NOOP":
			conn.Write(smtpOk250)
		case "MAIL":
			srv.connNumberMutex.RLock()
			if srv.connNumber <= srv.closeFirst {
				// Half of the time, close cleanly to simulate the server side closing
				// unexpectedly.
				if srv.connNumber%2 == 0 {
					log.Printf(
						"mail-test-srv: connection # %d < -closeFirst parameter %d, disconnecting client. Bye!\n",
						srv.connNumber, srv.closeFirst)
					clearState()
					conn.Close()
				} else {
					// The rest of the time, simulate a stale connection timeout by sending
					// a SMTP 421 message. This replicates the timeout/close from issue
					// 2249 - https://github.com/letsencrypt/boulder/issues/2249
					log.Printf(
						"mail-test-srv: connection # %d < -closeFirst parameter %d, disconnecting with 421. Bye!\n",
						srv.connNumber, srv.closeFirst)
					clearState()
					conn.Write([]byte("421 1.2.3 foo.bar.baz Error: timeout exceeded \r\n"))
					conn.Close()
				}
			}
			srv.connNumberMutex.RUnlock()
			clearState()
			matches := mailFromRegex.FindStringSubmatch(line)
			if matches == nil {
				log.Panicf("mail-test-srv: %s: MAIL FROM parse error\n", conn.RemoteAddr())
			}
			addr, err := mail.ParseAddress(matches[1])
			if err != nil {
				log.Panicf("mail-test-srv: %s: addr parse error: %v\n", conn.RemoteAddr(), err)
			}
			fromAddr = addr.Address
			conn.Write(smtpOk250)
		case "RCPT":
			matches := rcptToRegex.FindStringSubmatch(line)
			if matches == nil {
				conn.Write(smtpErr501)
				continue
			}
			addr, err := mail.ParseAddress(matches[1])
			if err != nil {
				log.Panicf("mail-test-srv: %s: addr parse error: %v\n", conn.RemoteAddr(), err)
			}
			toAddr = append(toAddr, addr.Address)
			conn.Write(smtpOk250)
		case "DATA":
			conn.Write([]byte("354 Start mail input \r\n"))
			var msgBuf bytes.Buffer

			for reader.Scan() {
				line := reader.Text()
				msgBuf.WriteString(line)
				msgBuf.WriteString("\r\n")
				if strings.HasSuffix(msgBuf.String(), "\r\n.\r\n") {
					break
				}
			}
			if reader.Err() != nil {
				log.Printf("mail-test-srv: read from %s: %v\n", conn.RemoteAddr(), reader.Err())
				return
			}

			mailResult := rcvdMail{
				From: fromAddr,
				Mail: msgBuf.String(),
			}
			srv.allMailMutex.Lock()
			for _, rcpt := range toAddr {
				mailResult.To = rcpt
				srv.allReceivedMail = append(srv.allReceivedMail, mailResult)
				log.Printf("mail-test-srv: Got mail: %s -> %s\n", fromAddr, rcpt)
			}
			srv.allMailMutex.Unlock()
			conn.Write([]byte("250 Got mail \r\n"))
			clearState()
		}
	}
	if reader.Err() != nil {
		log.Printf("mail-test-srv: read from %s: %s\n", conn.RemoteAddr(), reader.Err())
	}
}

func (srv *mailSrv) serveSMTP(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go srv.handleConn(conn)
	}
}

func main() {
	var listenAPI = flag.String("http", "0.0.0.0:9381", "http port to listen on")
	var listenSMTP = flag.String("smtp", "0.0.0.0:9380", "smtp port to listen on")
	var closeFirst = flag.Uint("closeFirst", 0, "close first n connections after MAIL for reconnection tests")

	flag.Parse()
	l, err := net.Listen("tcp", *listenSMTP)
	if err != nil {
		log.Fatalln("Couldn't bind %q for SMTP", *listenSMTP, err)
	}
	defer l.Close()

	srv := mailSrv{
		closeFirst: *closeFirst,
	}

	srv.setupHTTP(http.DefaultServeMux)
	go func() {
		err := http.ListenAndServe(*listenAPI, http.DefaultServeMux)
		if err != nil {
			log.Fatalln("Couldn't start HTTP server", err)
		}
	}()

	err = srv.serveSMTP(l)
	if err != nil {
		log.Fatalln(err, "Failed to accept connection")
	}
}
