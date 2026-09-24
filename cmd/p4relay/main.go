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

// wireCancellation cree le contexte racine de la passerelle et le raccorde au
// serveur HTTP, puis declare l'annulateur a la passerelle. Chaque requete
// entrante derive de BaseContext, donc de ce contexte : les appels amont bats
// sur r.Context() sont annules des que la fonction retournee est appelee, que
// ce soit par un signal ou par /api/shutdown via Gateway.StopUpstream. Sans ce
// raccordement, stop() n'annulait qu'un contexte que rien ne consultait et un
// appel amont bloque attendait la fin du delai fournisseur.
func wireCancellation(g *server.Gateway, srv *http.Server) context.CancelFunc {
	ctx, stop := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	g.SetStopUpstream(stop)
	return stop
}

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
	defer g.Journal().Stop() // dernier flush avant sortie, ne perd pas les entrees en attente
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
	stop := wireCancellation(g, srv)
	defer stop()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		// Annulation d'abord, fermeture ensuite : les handlers en cours voient
		// leurs requetes amont coupees immediatement, au lieu d'attendre la
		// fermeture brutale des connexions.
		stop()
		srv.Close()
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
