package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
)

type (
	attachmentsMsg struct {
		graphID string
		atts    []ipc.AttachmentInfo
		err     error
	}
	savedMsg struct {
		paths []string
		err   error
	}
)

// listAttachmentsCmd asks the daemon for the current message's attachments
// (they aren't cached), then opens the save picker.
func (m *Model) listAttachmentsCmd() tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok {
		return nil
	}
	if !msg.HasAttachments {
		m.errText = errNoAttachments.Error()
		return nil
	}
	cfg := m.cfg
	m.notice = "Listing attachments…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var atts []ipc.AttachmentInfo
		err := callDaemon(ctx, cfg, ipc.MethodAttachList, ipc.MessageRef{GraphID: msg.GraphID}, &atts)
		return attachmentsMsg{graphID: msg.GraphID, atts: atts, err: err}
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
	if len(r.atts) > 1 {
		var ids []string
		for _, a := range r.atts {
			ids = append(ids, a.ID)
		}
		p.all = append(p.all, choice{label: "All attachments", detail: humanSize(totalSize(r.atts)), attIDs: ids})
	}
	for _, a := range r.atts {
		detail := humanSize(int64(a.Size))
		if a.IsInline {
			detail += ", inline image"
		}
		p.all = append(p.all, choice{label: clean(a.Name), detail: detail, attIDs: []string{a.ID}})
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
	default:
		m.notice = "Saved " + strconv.Itoa(len(r.paths)) + " files to " + clean(strings.TrimSuffix(config.ExpandHome(m.cfg.UI.SaveDir), "/"))
	}
}
