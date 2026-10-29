package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// meetingMessage loads a cached meeting message and its calendar event
// (nil if the meeting isn't on the calendar).
func (d *daemon) meetingMessage(ctx context.Context, graphID string) (store.Message, *graph.Event, error) {
	if !d.cfg.Calendar.Enabled {
		return store.Message{}, nil, errors.New("calendar access is off (calendar.enabled = false)")
	}
	accountID, err := d.engine.Account(ctx)
	if err != nil {
		return store.Message{}, nil, err
	}
	msg, err := d.store.MessageByGraphID(ctx, accountID, graphID)
	if err != nil {
		return msg, nil, err
	}
	if msg.Meeting == nil {
		return msg, nil, errors.New("this message isn't a meeting invite")
	}
	ev, err := d.cal.MessageEvent(ctx, graphID)
	return msg, ev, err
}

func (d *daemon) meetingInfo(ctx context.Context, params json.RawMessage) (ipc.MeetingInfo, error) {
	var p ipc.MessageRef
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
		return ipc.MeetingInfo{}, errors.New("meeting needs a graph_id")
	}
	_, ev, err := d.meetingMessage(ctx, p.GraphID)
	if err != nil || ev == nil {
		return ipc.MeetingInfo{}, err
	}
	return ipc.MeetingInfo{
		Found:     true,
		Response:  ev.ResponseStatus.Response,
		Cancelled: ev.IsCancelled,
		Organizer: ev.IsOrganizer,
		JoinURL:   ev.JoinURL(),
	}, nil
}

// respond answers an invite, or removes a cancelled meeting from the
// calendar, then does calendar.after_respond to the message.
func (d *daemon) respond(ctx context.Context, params json.RawMessage) (ipc.RespondResult, error) {
	var p ipc.RespondParams
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
		return ipc.RespondResult{}, errors.New("respond needs a graph_id")
	}
	msg, ev, err := d.meetingMessage(ctx, p.GraphID)
	if err != nil {
		return ipc.RespondResult{}, err
	}
	switch p.Answer {
	case ipc.AnswerRemove:
		if ev != nil {
			if !ev.IsCancelled {
				return ipc.RespondResult{}, errors.New("the meeting hasn't been cancelled; decline it instead")
			}
			if err := d.cal.DeleteEvent(ctx, ev.ID); err != nil {
				return ipc.RespondResult{}, fmt.Errorf("removing the meeting: %w", err)
			}
		} // else: already off the calendar
	case ipc.AnswerAccept, ipc.AnswerTentative, ipc.AnswerDecline:
		if ev == nil {
			return ipc.RespondResult{}, errors.New("the meeting isn't on your calendar any more (it may have been cancelled or updated)")
		}
		if ev.IsCancelled {
			return ipc.RespondResult{}, errors.New("the meeting was cancelled; remove it instead")
		}
		if ev.IsOrganizer {
			return ipc.RespondResult{}, errors.New("you organized this meeting")
		}
		send := p.Send && ev.ResponseRequested
		if send {
			if err := d.checkOrganizer(ctx, ev); err != nil {
				return ipc.RespondResult{}, err
			}
		}
		answer := map[string]string{ipc.AnswerAccept: graph.Accept, ipc.AnswerTentative: graph.Tentative, ipc.AnswerDecline: graph.Decline}[p.Answer]
		if err := d.cal.RespondToEvent(ctx, ev.ID, answer, p.Comment, send); err != nil {
			return ipc.RespondResult{}, fmt.Errorf("answering the invite: %w", err)
		}
		d.log.Info("answered invite", "answer", p.Answer, "sent", send)
	default:
		return ipc.RespondResult{}, fmt.Errorf("unknown answer %q", p.Answer)
	}

	var res ipc.RespondResult
	op := map[string]string{"archive": ipc.OpArchive, "delete": ipc.OpDelete}[d.cfg.Calendar.AfterRespond]
	if op == "" {
		return res, nil
	}
	if _, err := d.mutate(ctx, ipc.MutateParams{Op: op, GraphIDs: []string{msg.GraphID}}); err != nil {
		res.AfterError = err.Error()
	} else {
		res.After = d.cfg.Calendar.AfterRespond
	}
	return res, nil
}

// checkOrganizer applies the send guardrail to an answer: it's mail to
// the organizer.
func (d *daemon) checkOrganizer(ctx context.Context, ev *graph.Event) error {
	if d.cfg.Safety.SendAllowed() {
		return nil
	}
	acct, err := d.store.DefaultAccount(ctx)
	if err != nil {
		return err
	}
	addr := ""
	if ev.Organizer != nil {
		addr = ev.Organizer.EmailAddress.Address
	}
	if !d.cfg.Safety.MaySendTo(addr, acct.UPN) {
		return fmt.Errorf("guardrail: sending is limited to yourself and safety.send_allow, so the answer can't go to %s (answer without telling the organizer, or set safety.allow_send = true)", addr)
	}
	return nil
}
