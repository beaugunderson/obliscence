package main

import (
	"os"
	"runtime/pprof"

	"github.com/alecthomas/kong"
)

var cli struct {
	DB      string `help:"Database path." default:"~/.obliscence/db.sqlite" env:"OBLISCENCE_DB"`
	JSON    bool   `help:"Output JSON." name:"json"`
	Profile string `help:"Write CPU profile to file." hidden:""`

	Index    IndexCmd    `cmd:"" help:"Index new/changed sessions."`
	Search   SearchCmd   `cmd:"" help:"Search conversations."`
	Sessions SessionsCmd `cmd:"" help:"List sessions."`
	Show     ShowCmd     `cmd:"" help:"Show a session conversation."`
	Resume   ResumeCmd   `cmd:"" help:"Resume a session in Claude Code."`
	Stats    StatsCmd    `cmd:"" help:"Database statistics."`
	Projects ProjectsCmd `cmd:"" help:"List projects."`
	Setup    SetupCmd    `cmd:"" help:"Download ONNX model for semantic search."`
	Hook     HookCmd     `cmd:"" help:"Handle Claude Code hook invocation." hidden:""`
}

func main() {
	ctx := kong.Parse(&cli,
		kong.Name("obliscence"),
		kong.Description("Archive and search Claude Code conversations."),
		kong.UsageOnError(),
	)

	if cli.Profile != "" {
		f, err := os.Create(cli.Profile)
		ctx.FatalIfErrorf(err)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	dbPath := expandPath(cli.DB)
	if err := os.MkdirAll(dirOf(dbPath), 0o755); err != nil {
		ctx.FatalIfErrorf(err)
	}

	db, err := openDB(dbPath)
	ctx.FatalIfErrorf(err)
	defer db.Close()

	err = ctx.Run(&RunContext{
		DB:   db,
		JSON: cli.JSON,
	})
	ctx.FatalIfErrorf(err)
}
