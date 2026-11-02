// Command fastmail-mcp serves Fastmail Masked Email to MCP clients.
//
// It reads a Fastmail API token with the Masked Email scope from
// FASTMAIL_TOKEN. By default it speaks MCP over stdio. With -http it serves
// streamable HTTP at /mcp instead, and every request must carry
// "Authorization: Bearer $MCP_AUTH_TOKEN"; GET /healthz stays open.
//
// Logs are JSON on stderr. Neither token is ever logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ionut-maxim/fastmail"
	"github.com/ionut-maxim/fastmail/jmap"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ionut-maxim/fastmail-mcp/internal/mcpserver"
)

func main() {
	addr := flag.String("http", "", "serve streamable HTTP on this address, such as :8080, instead of stdio")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(mcpserver.Version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, log); err != nil {
		log.Error("fastmail-mcp stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr string, log *slog.Logger) error {
	// Secrets mounted from files or pasted into env often end in a newline.
	token := strings.TrimSpace(os.Getenv("FASTMAIL_TOKEN"))
	if token == "" {
		return errors.New("set FASTMAIL_TOKEN to a Fastmail API token with the Masked Email scope")
	}
	authToken := strings.TrimSpace(os.Getenv("MCP_AUTH_TOKEN"))
	if addr != "" && authToken == "" {
		return errors.New("refusing to serve HTTP without MCP_AUTH_TOKEN: set it to a long random string, such as from openssl rand -hex 32")
	}

	client, err := fastmail.New(token, jmap.WithUserAgent("fastmail-mcp/"+mcpserver.Version))
	if err != nil {
		return err
	}
	if addr == "" {
		return serveStdio(ctx, client, log)
	}
	return serveHTTP(ctx, addr, client, authToken, log)
}

func serveStdio(ctx context.Context, client *fastmail.Client, log *slog.Logger) error {
	log.Info("serving stdio", "version", mcpserver.Version)
	err := mcpserver.New(client.MaskedEmail, log).Run(ctx, &mcp.StdioTransport{})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func serveHTTP(ctx context.Context, addr string, client *fastmail.Client, authToken string, log *slog.Logger) error {
	h, err := mcpserver.NewHTTPHandler(client.MaskedEmail, authToken, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- srv.Shutdown(shutdownCtx)
	}()
	log.Info("serving http", "addr", addr, "version", mcpserver.Version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}
