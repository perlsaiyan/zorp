package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/store"
)

// staleAfter: a calendar synced longer ago than this is probably not
// being kept up to date (the daemon syncs every minute or so).
const staleAfter = 15 * time.Minute

func runAgenda(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agenda", flag.ContinueOnError)
	days := fs.Int("days", 7, "how many days to show, starting today")
	asJSON := fs.Bool("json", false, "print the events as JSON")
	waybar := fs.Bool("waybar", false, "print a Waybar custom module's JSON (the meeting now, or the next one today)")
	all := fs.Bool("all", false, "include declined and cancelled meetings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, account, err := openCache(ctx, cfg)
	if err != nil {
		if *waybar {
			return printJSON(agenda.WaybarOutput{Class: "none", Alt: "none", Tooltip: "zorp has no mail cache yet"})
		}
		return fmt.Errorf("no local cache yet: run zorp first (%w)", err)
	}
	defer st.Close()

	sync, err := st.CalendarSync(ctx, account.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	stale := now.Sub(sync.SyncedAt) > staleAfter
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := today.AddDate(0, 0, max(*days, 1))
	if *waybar {
		end = today.AddDate(0, 0, 1)
	}
	events, err := st.EventsBetween(ctx, account.ID, now, end)
	if err != nil {
		return err
	}
	var shown []store.Event
	for _, e := range events {
		if agenda.Shown(e, *all) {
			shown = append(shown, e)
		}
	}

	switch {
	case *waybar:
		return printJSON(agenda.Waybar(shown, now, stale))
	case *asJSON:
		return printJSON(agenda.Items(shown))
	}
	if sync.SyncedAt.IsZero() {
		fmt.Fprintln(os.Stderr, "The calendar hasn't been synced yet. The daemon syncs it once zorp has calendar access (zorp probe checks).")
		return nil
	}
	fmt.Print(agenda.Text(shown, now))
	if stale {
		fmt.Fprintf(os.Stderr, "\n(calendar last synced %s; is the daemon running?)\n", sync.SyncedAt.Local().Format("Mon 15:04"))
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
