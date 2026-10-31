package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var version = "dev"
var revision = "unknown"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("imgserver stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := f.String("config", "config.json", "configuration file")
	issuer := f.String("issuer", "auth", "backend issuer")
	subject := f.String("subject", "user-1", "acting user ID")
	tenant := f.String("tenant", "default", "tenant namespace")
	resource := f.String("resource", "", "resource the backend has authorized, e.g. user:123 or ticket:456")
	profile := f.String("profile", "avatar", "upload profile")
	alias := f.String("alias", "", "public alias (avatar uses lowercase MD5 of an email or project:<uuid>)")
	action := f.String("action", "upload", "upload, read, or delete")
	fileID := f.String("file", "", "file ID for read/delete")
	maxBytes := f.Int64("max-bytes", 0, "optional tighter upload limit")
	ttl := f.Duration("ttl", 5*time.Minute, "grant lifetime, maximum 15m")
	receiptToken := f.String("receipt", "", "receipt JWT to verify")
	if err := f.Parse(args); err != nil {
		return err
	}
	if cmd == "version" {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"version": version, "revision": revision})
	}
	if cmd == "init" {
		cfg := defaultConfig()
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		out, err := os.OpenFile(*configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err = out.Write(append(b, '\n')); err != nil {
			out.Close()
			return err
		}
		if err = out.Close(); err != nil {
			return err
		}
		fmt.Printf("Created %s with a random signing key. Start with: go run . serve -config %s\n", *configPath, *configPath)
		return nil
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	switch cmd {
	case "healthcheck":
		return healthcheck(c)
	case "token":
		if *resource == "" || *ttl <= 0 || *ttl > 15*time.Minute {
			return errors.New("resource is required; ttl must be positive and at most 15m")
		}
		if *action != "upload" && *action != "read" && *action != "delete" {
			return errors.New("action must be upload, read, or delete")
		}
		now := time.Now()
		claims := Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: *issuer, Subject: *subject, Audience: jwt.ClaimStrings{"imgserver"}, ID: randomID(18), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(*ttl))}, Action: *action, Tenant: *tenant, Resource: *resource, Profile: *profile, Alias: *alias, FileID: *fileID, MaxBytes: *maxBytes}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(c.SigningKey))
		if err != nil {
			return err
		}
		if _, err = (&Server{cfg: c}).parseGrant(token, *action); err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	case "verify-receipt":
		var receipt Receipt
		_, err := jwt.ParseWithClaims(*receiptToken, &receipt, func(t *jwt.Token) (any, error) { return derivedKey(c.SigningKey, "receipt"), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("imgserver"), jwt.WithAudience(*issuer), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	case "serve":
		return serve(c)
	default:
		return fmt.Errorf("unknown command %q (use init, serve, token, verify-receipt, healthcheck, or version)", cmd)
	}
}

func healthcheck(c Config) error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func serve(c Config) error {
	m, err := openMetadata(c.Database)
	if err != nil {
		return err
	}
	defer m.db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := newObjectStore(ctx, c.Storage)
	if err != nil {
		return err
	}
	app := newServer(c, m, store)
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			cleanCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := app.cleanup(cleanCtx); err != nil && ctx.Err() == nil {
				slog.Error("object cleanup failed; will retry", "error", err)
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { stop(); <-cleanupDone }()
	srv := &http.Server{Addr: c.Listen, Handler: app.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Error("shutdown failed", "error", err)
			srv.Close()
		}
	}()
	slog.Info("imgserver listening", "address", c.Listen, "base_url", c.BaseURL, "storage", c.Storage.Driver)
	err = srv.ListenAndServe()
	stop()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
