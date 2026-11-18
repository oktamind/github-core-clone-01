// forge: the Go git forge binary. Binds an ephemeral port on all
// interfaces, prints "PORT <n>" on stdout for the Node launcher, then serves
// until SIGTERM. The launcher (main.ts) owns register/poll-done with the env.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("PORT %d\n", addr.Port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(c)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	_ = ln.Close()
}
