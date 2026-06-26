// Command dbserver starts a db-engine TCP server.
//
// Usage:
//
//	dbserver [-dir <datadir>] [-port <port>]
//
// Flags:
//
//	-dir   path to the database directory (default: ".")
//	-port  TCP port to listen on          (default: 5433)
//
// Example:
//
//	dbserver -dir ./mydb -port 5433
//
// Once running, connect with the client library:
//
//	c, _ := client.Dial("localhost:5433")
//	res, _ := c.Exec("SELECT * FROM users")
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/yahya/db-engine/executor"
	"github.com/yahya/db-engine/server"
)

func main() {
	dir := flag.String("dir", ".", "database directory")
	port := flag.String("port", "5433", "TCP port to listen on")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatalf("mkdir %q: %v", *dir, err)
	}

	db, err := executor.Open(*dir)
	if err != nil {
		log.Fatalf("open db %q: %v", *dir, err)
	}
	defer db.Close()

	ln, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Fatalf("listen :%s: %v", *port, err)
	}

	srv := server.New(db)

	// Handle SIGINT / SIGTERM: drain connections then exit cleanly.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nshutting down…")
		srv.Close()
	}()

	fmt.Printf("db-engine listening on %s  (database: %s)\n", ln.Addr(), *dir)
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
