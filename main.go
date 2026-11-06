package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/robertkozin/discord-video-preview-bot/bot"
	"github.com/robertkozin/discord-video-preview-bot/browser"
	"github.com/robertkozin/discord-video-preview-bot/canary"
	"github.com/robertkozin/discord-video-preview-bot/preview"
	"github.com/robertkozin/discord-video-preview-bot/tr"
)

type Args struct {
	Destination  *url.URL   `env:"DESTINATION" envDefault:"fs://_data/server/?server=localhost:8080"`
	Extractors   []*url.URL `env:"EXTRACTORS" envDefault:"cobalt://localhost:9000?insecure=1"`
	PublicURL    *url.URL   `env:"PUBLIC_URL" envDefault:"http://localhost:8080"`
	DiscordToken string     `env:"DISCORD_TOKEN"`

	// HTTPAddr serves the debug page (paste a url, recent canary checks). It
	// defaults to 0.0.0.0:8081 when there is no DISCORD_TOKEN, and is off
	// otherwise. Publish it on a private address only.
	HTTPAddr string `env:"HTTP_ADDR"`
	// GrafanaURL makes the debug page link each canary check to its trace.
	GrafanaURL string `env:"GRAFANA_URL"`

	// CanarySources enables ongoing testing, e.g. reddit:// (see package canary).
	CanarySources []*url.URL `env:"CANARY_SOURCE"`
	// CanaryInterval is the gap between checks, CanaryRefresh the gap between
	// fetching new links to check.
	CanaryInterval time.Duration `env:"CANARY_INTERVAL" envDefault:"10m"`
	CanaryRefresh  time.Duration `env:"CANARY_REFRESH" envDefault:"30m"`
}

func main() {
	ctx := context.Background()

	args, err := parseArgs()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error parsing startup args: %+v\n", err)
		os.Exit(1)
	}

	err = run(ctx, args)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error during startup: %+v\n", err)
		os.Exit(1)
	}
}

func parseArgs() (args Args, err error) {
	err = env.Parse(&args)
	if err != nil {
		return
	}

	// todo: override default somehow idk
	for i := 1; i <= 10; i++ {
		key := fmt.Sprintf("EXTRACTOR_%d", i)
		raw := os.Getenv(key)
		if raw != "" {
			u, err := url.Parse(raw)
			if err != nil {
				return args, fmt.Errorf("parsing %s=%s as a url: %w", key, raw, err)
			}
			args.Extractors = append(args.Extractors, u)
		}
	}

	return
}

func run(ctx context.Context, args Args) error {
	var err error
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	defer tr.Shutdown()

	stopBrowser := browser.Start()
	defer stopBrowser()

	extractors := make([]preview.Extractor, len(args.Extractors))
	for i, ex := range args.Extractors {
		extractors[i], err = preview.NewExtractor(ex)
		if err != nil {
			return fmt.Errorf("creating extractor: %w", err)
		}
	}

	dest, err := preview.NewDestination(ctx, args.Destination)
	if err != nil {
		return fmt.Errorf("creating destination: %w", err)
	}

	reuploader := preview.Reuploader{
		PublicURL:   args.PublicURL.String(),
		Extractors:  extractors,
		Destination: dest,
	}

	var runner *canary.Runner
	if len(args.CanarySources) > 0 {
		var sources []canary.Source
		for _, u := range args.CanarySources {
			switch u.Scheme {
			case "reddit":
				source, err := canary.NewReddit(u)
				if err != nil {
					return fmt.Errorf("creating canary source: %w", err)
				}
				sources = append(sources, source)
			default:
				return fmt.Errorf("unknown canary source: %s", u.Scheme)
			}
		}
		runner = &canary.Runner{
			Sources: sources,
			// same extractors as the real bot, but nothing is stored and the
			// manifest cache can never short-circuit a check
			Reuploader: &preview.Reuploader{
				Extractors:  extractors,
				Destination: preview.DiscardDestination{},
				PublicURL:   "http://canary.invalid",
			},
			Interval:        args.CanaryInterval,
			RefreshInterval: args.CanaryRefresh,
		}
		go runner.Run(ctx)
	}

	if args.DiscordToken != "" {
		bot := &bot.Discord{
			Token:      args.DiscordToken,
			Reuploader: reuploader,
		}
		err := bot.Start()
		if err != nil {
			return fmt.Errorf("starting discord bot: %w", err)
		}
		fmt.Println("discord running")
		defer bot.Close()
	}

	httpAddr := args.HTTPAddr
	if httpAddr == "" && args.DiscordToken == "" {
		httpAddr = "0.0.0.0:8081"
	}
	if httpAddr != "" {
		var items func() []canary.Item
		if runner != nil {
			items = runner.Items
		}
		handler := bot.SimpleServer(&reuploader, items, args.GrafanaURL)
		go func() {
			if err := http.ListenAndServe(httpAddr, handler); err != nil {
				fmt.Fprintf(os.Stderr, "debug server: %v\n", err)
			}
		}()
		fmt.Println("debug page on", httpAddr)
	}

	fmt.Println("running!")
	<-ctx.Done()
	fmt.Println("done!")

	return nil
}
