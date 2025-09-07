package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/caarlos0/env/v11"
	"github.com/robertkozin/discord-video-preview-bot/bot"
	"github.com/robertkozin/discord-video-preview-bot/preview"
)

type Args struct {
	Destination  *url.URL   `env:"DESTINATION" envDefault:"rclone+webdav://localhost:8080"`
	Extractors   []*url.URL `env:"EXTRACTORS" envDefault:"cobalt://localhost:9000?insecure=1"`
	PublicURL    *url.URL   `env:"PUBLIC_URL" envDefault:"http://localhost:8080"`
	DiscordToken string     `env:"DISCORD_TOKEN"`
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

func parseArgs() (Args, error) {
	return env.ParseAs[Args]()
}

func run(ctx context.Context, args Args) error {
	var err error
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tracerCleanup, err := initTracer("discord-video-preview-bot")
	if err != nil {
		return fmt.Errorf("initializing tracer: %w", err)
	}
	defer tracerCleanup()

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

	if args.DiscordToken != "" {
		bot := &bot.Discord{
			Token:      args.DiscordToken,
			Reuploader: reuploader,
		}
		err := bot.Start()
		if err != nil {
			return fmt.Errorf("starting discord bot: %w", err)
		}
		defer bot.Close()
	} else {
		handler := bot.SimpleServer(&reuploader)
		go http.ListenAndServe("localhost:8081", handler)
	}

	fmt.Println("running!")
	<-ctx.Done()

	return nil
}
