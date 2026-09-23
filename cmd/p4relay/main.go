// Commande p4relay : passerelle IA locale P4Relay.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p4relay/internal/p4relay/server"
)

func main() {
	dataDir := os.Getenv("P4_DATA_DIR")
	if dataDir == "" {
		exe, _ := os.Executable()
		dataDir = filepath.Join(filepath.Dir(exe), "data")
	}
	g, err := server.New(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	g.Journal().Load()
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", g.Host(), g.Port()))
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			log.Fatalf("Le port %d est d\u00e9j\u00e0 utilis\u00e9. Ouvrez http://127.0.0.1:%d ou choisissez un autre PORT.", g.Port(), g.Port())
		}
		log.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           http.HandlerFunc(g.Handle),
	}
	g.SetServer(srv)
	_, stop := context.WithCancel(context.Background())
	g.SetStopUpstream(stop)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		srv.Close()
		stop()
	}()
	fmt.Printf("P4Relay\nInterface : http://127.0.0.1:%d\nAPI       : http://127.0.0.1:%d/v1\n\u00c9coute    : %s:%d\nCtrl+C pour arr\u00eater.\n", g.Port(), g.Port(), g.Host(), g.Port())
	if len(os.Args) > 1 && os.Args[1] == "--open-browser" {
		cmd := exec.Command("explorer.exe", fmt.Sprintf("http://127.0.0.1:%d", g.Port()))
		_ = cmd.Start()
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
