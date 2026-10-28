package tui

import (
	"encoding/json"
	"os"

	tea "charm.land/bubbletea/v2"
)

// divider is a pane border being dragged.
type divider int

const (
	noDivider divider = iota
	folderDivider
	previewDivider // between list and preview, right layout
	bottomDivider  // between list and preview, bottom layout
)

// uiState is what's remembered between runs.
type uiState struct {
	FolderW     int     `json:"folder_w,omitempty"`
	ListShare   float64 `json:"list_share,omitempty"`
	ListHShare  float64 `json:"list_h_share,omitempty"`
	Layout      layout  `json:"layout"`
	HideFolders bool    `json:"hide_folders,omitempty"`
}

func (m *Model) loadUIState() {
	data, err := os.ReadFile(m.cfg.Paths.UIState)
	if err != nil {
		return
	}
	var s uiState
	if json.Unmarshal(data, &s) != nil {
		return
	}
	m.folderW, m.listShare, m.listHShare = s.FolderW, s.ListShare, s.ListHShare
	if s.Layout >= previewRight && s.Layout <= previewHidden {
		m.layout = s.Layout
	}
	m.showFolders = !s.HideFolders
}

// saveUIState remembers layout choices. Failures are ignored: it's a
// convenience, not data.
func (m *Model) saveUIState() {
	if m.cfg.Paths.UIState == "" {
		return
	}
	data, _ := json.Marshal(uiState{
		FolderW: m.folderW, ListShare: m.listShare, ListHShare: m.listHShare,
		Layout: m.layout, HideFolders: !m.showFolders,
	})
	_ = os.WriteFile(m.cfg.Paths.UIState, data, 0o600)
}

// hitDivider reports which divider, if any, is at screen cell x, y.
func (m *Model) hitDivider(x, y int) divider {
	if m.reading || m.help || m.picker != nil || y < 1 || y > m.height-2 {
		return noDivider
	}
	g := m.geom()
	listX := 0
	if g.folderW > 0 {
		if x == g.folderW {
			return folderDivider
		}
		listX = g.folderW + 1
	}
	if g.previewOnRight && x == listX+g.listW {
		return previewDivider
	}
	if g.previewH > 0 && !g.previewOnRight && x >= listX && y == 1+g.listH {
		return bottomDivider
	}
	return noDivider
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	mouse := msg.Mouse()
	if m.viewer != nil {
		if _, ok := msg.(tea.MouseWheelMsg); ok {
			if mouse.Button == tea.MouseWheelUp {
				m.viewer.vp.ScrollUp(3)
			} else if mouse.Button == tea.MouseWheelDown {
				m.viewer.vp.ScrollDown(3)
			}
		}
		return nil
	}
	switch msg.(type) {
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return nil
		}
		if d := m.hitDivider(mouse.X, mouse.Y); d != noDivider {
			m.dragging = d
			return nil
		}
		return m.click(mouse.X, mouse.Y)

	case tea.MouseMotionMsg:
		if m.dragging == noDivider {
			return nil
		}
		m.drag(mouse.X, mouse.Y)
		return nil

	case tea.MouseReleaseMsg:
		if m.dragging != noDivider {
			m.dragging = noDivider
			m.saveUIState()
		}
		return nil

	case tea.MouseWheelMsg:
		step := 3
		if mouse.Button == tea.MouseWheelUp {
			step = -3
		} else if mouse.Button != tea.MouseWheelDown {
			return nil
		}
		switch m.paneAt(mouse.X, mouse.Y) {
		case paneFolders:
			m.folderCur = clamp(m.folderCur+step, 0, len(m.folders)-1)
		case panePreview:
			if step > 0 {
				m.preview.ScrollDown(step)
			} else {
				m.preview.ScrollUp(-step)
			}
		default:
			if m.reading {
				if step > 0 {
					m.preview.ScrollDown(step)
				} else {
					m.preview.ScrollUp(-step)
				}
				return nil
			}
			return m.moveMessage(step)
		}
	}
	return nil
}

// drag moves the divider being dragged to x, y.
func (m *Model) drag(x, y int) {
	g := m.geom()
	listX := 0
	if g.folderW > 0 {
		listX = g.folderW + 1
	}
	switch m.dragging {
	case folderDivider:
		m.folderW = clamp(x, 10, max(m.width/2, 10))
	case previewDivider:
		rest := m.width - listX
		if rest > 0 {
			m.listShare = float64(clamp(x-listX, 20, rest-21)) / float64(rest)
		}
	case bottomDivider:
		if g.bodyH > 0 {
			m.listHShare = float64(clamp(y-1, 3, g.bodyH-4)) / float64(g.bodyH)
		}
	}
	m.refreshPreview(false)
}

// paneAt reports which pane covers screen cell x, y.
func (m *Model) paneAt(x, y int) pane {
	g := m.geom()
	listX := 0
	if g.folderW > 0 {
		if x < g.folderW {
			return paneFolders
		}
		listX = g.folderW + 1
	}
	switch {
	case g.previewOnRight && x > listX+g.listW:
		return panePreview
	case g.previewH > 0 && !g.previewOnRight && y > 1+g.listH:
		return panePreview
	}
	return paneList
}

func (m *Model) click(x, y int) tea.Cmd {
	if m.reading || m.help || m.picker != nil || y < 1 || y > m.height-2 {
		return nil
	}
	row := y - 1
	switch p := m.paneAt(x, y); p {
	case paneFolders:
		m.focus = paneFolders
		idx := clamp(m.folderCur-m.geom().bodyH/2, 0, max(len(m.folders)-m.geom().bodyH, 0)) + row
		if idx < len(m.folders) {
			m.folderCur = idx
			return m.openFolder(m.folders[idx].ID)
		}
	case paneList:
		m.focus = paneList
		if idx := m.msgTop + row; idx < len(m.msgs) && idx != m.msgCur {
			m.msgCur = idx
			return m.selectMessage()
		}
	case panePreview:
		m.focus = panePreview
	}
	return nil
}
