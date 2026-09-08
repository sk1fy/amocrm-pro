package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
)

const usage = "usage: integrations <create|update|disable|enable|rotate-secret|set-service> --actor ID --code CODE [--client-id UUID --redirect-uri HTTPS_URL --webhook-events CSV --services CSV|none --secret-stdin --service CODE --enabled true|false]"

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	command, err := parseCommand(args, input)
	if err != nil {
		return err
	}
	defer clear(command.Secret)
	cfg, err := config.LoadOperator()
	if err != nil {
		return errors.New("invalid operator environment; check database and encryption configuration")
	}
	keys, err := cryptox.ParseKeyRing(cfg.EncryptionKeys, cfg.EncryptionKeyVersion)
	if err != nil {
		return errors.New("invalid encryption keyring")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.ServiceName, 1)
	if err != nil {
		return errors.New("connect operator database failed")
	}
	defer pool.Close()
	result, err := integrations.NewStore(pool, keys).Apply(ctx, command)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func parseCommand(args []string, input io.Reader) (integrations.Command, error) {
	c := integrations.Command{}
	if len(args) == 0 {
		return c, errors.New(usage)
	}
	c.Action = args[0]
	flags := flag.NewFlagSet("integrations", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&c.Actor, "actor", "", "")
	flags.StringVar(&c.Code, "code", "", "")
	flags.StringVar(&c.ClientID, "client-id", "", "")
	redirect := flags.String("redirect-uri", "", "")
	events := flags.String("webhook-events", "", "")
	grants := flags.String("services", "", "")
	flags.StringVar(&c.Service, "service", "", "")
	enabled := flags.String("enabled", "", "")
	secret := flags.Bool("secret-stdin", false, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return c, errors.New(usage)
	}
	seen := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["redirect-uri"] {
		c.RedirectURI = redirect
	}
	if seen["webhook-events"] {
		v := csv(*events)
		c.WebhookEvents = &v
	}
	if seen["services"] {
		if *grants == "none" {
			c.Services = []string{}
		} else {
			c.Services = csv(*grants)
			if len(c.Services) == 0 {
				return c, errors.New("services must be a non-empty CSV list or none")
			}
		}
	}
	if c.Action == "set-service" {
		if *enabled != "true" && *enabled != "false" {
			return c, errors.New("set-service requires --enabled true or false")
		}
		c.Enabled = *enabled == "true"
	} else if seen["enabled"] {
		return c, errors.New("enabled is only accepted for set-service")
	}
	if (c.Action == "create" || c.Action == "rotate-secret") != *secret {
		return c, errors.New("create and rotate-secret require --secret-stdin; other commands do not accept it")
	}
	if *secret {
		// A single trailing line ending is tolerated for secret-manager output.
		raw, err := io.ReadAll(io.LimitReader(input, 16387))
		if err != nil {
			clear(raw)
			return c, errors.New("read client secret failed")
		}
		raw = stringsTrimLineEnding(raw)
		if len(raw) > 16384 {
			clear(raw)
			return c, errors.New("client secret exceeds 16384 bytes")
		}
		c.Secret = raw
	}
	if err := c.Validate(); err != nil {
		clear(c.Secret)
		return integrations.Command{}, err
	}
	return c, nil
}

func stringsTrimLineEnding(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
	}
	return raw
}
func csv(value string) []string {
	result := []string{}
	for _, part := range strings.Split(value, ",") {
		if item := strings.TrimSpace(part); item != "" {
			result = append(result, item)
		}
	}
	return result
}
