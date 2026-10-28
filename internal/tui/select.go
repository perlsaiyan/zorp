package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/internal/ipc"
)

// Selecting several messages (space, *) to archive, delete, move, mark
// read or flag them together. The selection is by graph ID and only
// covers messages in the current list.

// toggleSelected selects or unselects the message under the cursor and
// moves to the next one.
func (m *Model) toggleSelected() tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok {
		return nil
	}
	if m.selected[msg.GraphID] {
		delete(m.selected, msg.GraphID)
	} else {
		if m.selected == nil {
			m.selected = map[string]bool{}
		}
		m.selected[msg.GraphID] = true
	}
	return m.moveMessage(1)
}

// selectAll selects every listed message, or clears the selection if
// they're all selected already.
func (m *Model) selectAll() {
	if len(m.msgs) > 0 && len(m.selected) == len(m.msgs) {
		m.selected = nil
		return
	}
	m.selected = make(map[string]bool, len(m.msgs))
	for _, msg := range m.msgs {
		m.selected[msg.GraphID] = true
	}
}

// pruneSelection drops selected messages that are no longer listed.
func (m *Model) pruneSelection() {
	if len(m.selected) == 0 {
		return
	}
	listed := make(map[string]bool, len(m.msgs))
	for _, msg := range m.msgs {
		listed[msg.GraphID] = true
	}
	for id := range m.selected {
		if !listed[id] {
			delete(m.selected, id)
		}
	}
}

// selectedIDs lists the selection in list order.
func (m *Model) selectedIDs() []string {
	var out []string
	for _, msg := range m.msgs {
		if m.selected[msg.GraphID] {
			out = append(out, msg.GraphID)
		}
	}
	return out
}

// selectionKey applies an action key to the selection. ok is false for
// keys that act on the current message only (reply, open…).
func (m *Model) selectionKey(key string) (tea.Cmd, bool) {
	ids := m.selectedIDs()
	n := strconv.Itoa(len(ids)) + " messages"
	if len(ids) == 1 {
		n = "1 message"
	}
	op := func(op string) tea.Cmd {
		return mutateCmd(m.cfg, ipc.MutateParams{Op: op, GraphIDs: ids})
	}
	ask := func(prompt, o string) (tea.Cmd, bool) {
		m.confirm = &confirm{prompt: prompt + " " + n + "? y/n", cmd: op(o), clearSelection: true}
		return nil, true
	}
	switch key {
	case "u":
		o := ipc.OpUnread
		for _, msg := range m.msgs {
			if m.selected[msg.GraphID] && !msg.IsRead {
				o = ipc.OpRead // any unread: mark them all read
				break
			}
		}
		m.selected = nil
		return op(o), true
	case "f":
		o := ipc.OpUnflag
		for _, msg := range m.msgs {
			if m.selected[msg.GraphID] && !msg.Flagged {
				o = ipc.OpFlag
				break
			}
		}
		m.selected = nil
		return op(o), true
	case "a":
		return ask("Archive", ipc.OpArchive)
	case "d":
		if m.cfg.Keys.DeleteArchives {
			return ask("Archive", ipc.OpArchive)
		}
		return ask("Delete (move to Deleted Items)", ipc.OpDelete)
	case "X":
		return ask("Delete (move to Deleted Items)", ipc.OpDelete)
	case "m":
		m.openMovePicker(ids)
		return nil, true
	}
	return nil, false
}
