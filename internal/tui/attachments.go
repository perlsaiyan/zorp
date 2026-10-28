package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

type (
	attachmentsMsg struct {
		graphID string
		atts    []ipc.AttachmentInfo
		open    bool // the open picker (V), not the save picker (s)
		err     error
	}
	// cachedAttsMsg is a message's attachment list, fetched into the cache
	// because the body was cached before attachment lists were.
	cachedAttsMsg struct {
		seq  int
		atts []store.Attachment
	}
	savedMsg struct {
		paths []string
		why   string // shown after the path
		err   error
	}
)

// fetchAttsCmd has the daemon fetch (and cache) the current message's
// attachment list, for the preview header.
func (m *Model) fetchAttsCmd() tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok || !msg.HasAttachments || m.attsKnown {
		return nil
	}
	m.attsKnown = true // asked; don't ask again for this selection
	cfg, st, seq := m.cfg, m.st, m.bodySeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := callDaemon(ctx, cfg, ipc.MethodAttachList, ipc.MessageRef{GraphID: msg.GraphID}, nil); err != nil {
			return nil // the header just goes without; s and V still work
		}
		atts, _, err := st.Attachments(ctx, msg.ID)
		if err != nil {
			return nil
		}
		return cachedAttsMsg{seq: seq, atts: atts}
	}
}

// attachedLine lists the current message's attachments for the preview
// header; inline images are left out (they're in the body).
func (m *Model) attachedLine() string {
	var parts []string
	for _, a := range m.atts {
		if a.IsInline {
			continue
		}
		what := humanSize(a.Size)
		switch a.Kind {
		case "item":
			what = "email"
		case "reference":
			what = "link"
		}
		parts = append(parts, clean(a.Name)+" ("+what+")")
	}
	return strings.Join(parts, ", ")
}

// listAttachmentsCmd opens the attachment picker: for saving (s) or
// opening (V). The cached list is used when there is one; drafts change,
// so theirs is always asked for.
func (m *Model) listAttachmentsCmd(open bool) tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok {
		return nil
	}
	if !msg.HasAttachments && !(open && len(m.atts) > 0) {
		m.errText = errNoAttachments.Error()
		return nil
	}
	if m.attsKnown && len(m.atts) > 0 && msg.ID == m.bodyID && !m.isDraft(msg) {
		atts := make([]ipc.AttachmentInfo, 0, len(m.atts))
		for _, a := range m.atts {
			atts = append(atts, ipc.AttachmentInfo{ID: a.GraphID, Name: a.Name, ContentType: a.ContentType, Size: int(a.Size), IsInline: a.IsInline, Kind: a.Kind})
		}
		m.openAttachmentPicker(attachmentsMsg{graphID: msg.GraphID, atts: atts, open: open})
		return nil
	}
	cfg := m.cfg
	m.notice = "Listing attachments…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var atts []ipc.AttachmentInfo
		err := callDaemon(ctx, cfg, ipc.MethodAttachList, ipc.MessageRef{GraphID: msg.GraphID}, &atts)
		return attachmentsMsg{graphID: msg.GraphID, atts: atts, open: open, err: err}
	}
}

func (m *Model) openAttachmentPicker(r attachmentsMsg) {
	m.notice = ""
	if r.err != nil {
		m.errText = r.err.Error()
		return
	}
	if len(r.atts) == 0 {
		m.errText = errNoAttachments.Error()
		return
	}
	p := &picker{kind: pickAttach, title: "Save to " + m.cfg.UI.SaveDir, graphID: r.graphID}
	if r.open {
		p.kind, p.title = pickOpen, "Open attachment"
	} else if len(r.atts) > 1 {
		var ids []string
		for _, a := range r.atts {
			ids = append(ids, a.ID)
		}
		p.all = append(p.all, choice{label: "All attachments", detail: humanSize(totalSize(r.atts)), attIDs: ids})
	}
	// Inline images go last: they're already in the message.
	for _, inline := range []bool{false, true} {
		for _, a := range r.atts {
			if a.IsInline != inline {
				continue
			}
			detail := humanSize(int64(a.Size))
			switch {
			case a.Kind == "item":
				detail = "attached email, " + detail
			case a.Kind == "reference":
				detail = "link to a cloud file"
			case a.IsInline:
				detail += ", inline image"
			}
			p.all = append(p.all, choice{label: clean(a.Name), detail: detail, attIDs: []string{a.ID}, att: a})
		}
	}
	m.picker = p
	m.filterPicker()
}

func totalSize(atts []ipc.AttachmentInfo) int64 {
	var n int64
	for _, a := range atts {
		n += int64(a.Size)
	}
	return n
}

func saveAttachmentsCmd(cfg *config.Config, graphID string, ids []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var paths []string
		for _, id := range ids {
			var res ipc.SaveAttachmentResult
			if err := callDaemon(ctx, cfg, ipc.MethodSaveAttach, ipc.SaveAttachmentParams{GraphID: graphID, AttachmentID: id}, &res); err != nil {
				return savedMsg{paths: paths, err: err}
			}
			paths = append(paths, res.Path)
		}
		return savedMsg{paths: paths}
	}
}

func (m *Model) saved(r savedMsg) {
	m.notice = ""
	if r.err != nil {
		m.errText = "saving: " + r.err.Error()
		return
	}
	switch len(r.paths) {
	case 0:
	case 1:
		m.notice = "Saved " + clean(r.paths[0])
		if r.why != "" {
			m.notice += " (" + r.why + ")"
		}
	default:
		m.notice = "Saved " + strconv.Itoa(len(r.paths)) + " files to " + clean(strings.TrimSuffix(config.ExpandHome(m.cfg.UI.SaveDir), "/"))
	}
}
