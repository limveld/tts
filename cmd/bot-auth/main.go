// Command bot-auth runs the one-time Twitch OAuth consent for the bot and saves
// the resulting token to the store the bot reads (bot.tokens.json by default).
//
// Set TWITCH_CLIENT_ID and TWITCH_CLIENT_SECRET, run it, open the printed URL,
// and log in as the SENDING account (a dedicated bot account, or your own). The
// token refreshes unattended afterward, so this is only needed once.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"tts/twitch"
)

// Scopes needed to send chat messages as the authorized user.
var scopes = []string{"user:write:chat", "user:bot"}

func main() {
	redirect := flag.String("redirect", "http://localhost:3000", "OAuth redirect URL (must match the Twitch app exactly)")
	storePath := flag.String("store", "bot.tokens.json", "where to save the token (the bot's -twitch-token-store)")
	flag.Parse()

	clientID := os.Getenv("TWITCH_CLIENT_ID")
	secret := os.Getenv("TWITCH_CLIENT_SECRET")
	if clientID == "" || secret == "" {
		log.Fatal("set TWITCH_CLIENT_ID and TWITCH_CLIENT_SECRET (from your Twitch app)")
	}

	u, err := url.Parse(*redirect)
	if err != nil || u.Host == "" {
		log.Fatalf("invalid -redirect %q: %v", *redirect, err)
	}

	store := twitch.NewStore(*storePath)
	client := twitch.NewClient(clientID, secret, store)
	state := randHex()
	authURL := client.AuthCodeURL(*redirect, state, scopes)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	path := u.Path
	if path == "" {
		path = "/"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "Authorization failed: %s (%s). You can close this tab.", e, q.Get("error_description"))
			errCh <- fmt.Errorf("authorization denied: %s", e)
			return
		}
		code := q.Get("code")
		if code == "" {
			return // ignore stray requests (favicon, etc.)
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch (possible CSRF)")
			return
		}
		fmt.Fprint(w, "Authorized! You can close this tab and return to the terminal.")
		codeCh <- code
	})
	srv := &http.Server{Addr: u.Host, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Println("\nOpen this URL and approve (log in as the SENDING account):")
	fmt.Println("\n  " + authURL + "\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		log.Fatalf("auth: %v", err)
	case <-ctx.Done():
		log.Fatal("timed out waiting for authorization (5m)")
	}

	// Let the browser render the success page before we stop listening.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)

	tok, err := client.Exchange(ctx, code, *redirect)
	if err != nil {
		log.Fatalf("exchanging code: %v", err)
	}
	userID, login, err := client.Validate(ctx, tok.AccessToken)
	if err != nil {
		log.Fatalf("validating token: %v", err)
	}
	tok.UserID, tok.Login = userID, login
	if err := store.Save(tok); err != nil {
		log.Fatalf("saving token: %v", err)
	}

	fmt.Printf("Authorized as %s (user id %s). Token saved to %s.\n", login, userID, *storePath)
	fmt.Println("The bot will now reply in chat; the token refreshes automatically.")
}

func randHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("generating state: %v", err)
	}
	return hex.EncodeToString(b)
}
