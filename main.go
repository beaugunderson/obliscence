package main

import (
	"os"

	"github.com/alecthomas/kong"
)

var cli struct {
	DB   string `help:"Database path." default:"~/.obliscence/db.sqlite" env:"OBLISCENCE_DB"`
	JSON bool   `help:"Output JSON." name:"json"`

	Index    IndexCmd    `cmd:"" help:"Index new/changed sessions."`
	Search   SearchCmd   `cmd:"" help:"Search conversations."`
	Sessions SessionsCmd `cmd:"" help:"List sessions."`
	Show     ShowCmd     `cmd:"" help:"Show a session conversation."`
	Stats    StatsCmd    `cmd:"" help:"Database statistics."`
	Projects ProjectsCmd `cmd:"" help:"List projects."`
	Hook     HookCmd     `cmd:"" help:"Handle Claude Code hook invocation." hidden:""`
}

func main() {
	ctx := kong.Parse(&cli,
		kong.Name("obliscence"),
		kong.Description("Archive and search Claude Code conversations."),
		kong.UsageOnError(),
	)

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
