package main

import (
	"errors"
	"fmt"
)

type viewerInput struct {
	Type          string  `json:"type"`
	Key           int     `json:"key,omitempty"`
	Down          bool    `json:"down,omitempty"`
	MetaState     int     `json:"metaState,omitempty"`
	X             int     `json:"x,omitempty"`
	Y             int     `json:"y,omitempty"`
	Text          string  `json:"text,omitempty"`
	Event         int     `json:"event,omitempty"`
	PointerID     int     `json:"pointerId,omitempty"`
	Pressure      float64 `json:"pressure,omitempty"`
	MajorAxisSize float64 `json:"majorAxisSize,omitempty"`
}

type streamInputData struct {
	DisplayID     int     `json:"displayId"`
	Ver           int     `json:"ver,omitempty"`
	Caps          int     `json:"caps,omitempty"`
	Flags         int     `json:"flags,omitempty"`
	Key           int     `json:"key,omitempty"`
	Down          bool    `json:"down"`
	MetaState     int     `json:"metaState,omitempty"`
	X             int     `json:"x,omitempty"`
	Y             int     `json:"y,omitempty"`
	Text          string  `json:"text,omitempty"`
	Event         int     `json:"event,omitempty"`
	PointerID     int     `json:"pointerId,omitempty"`
	Pressure      float64 `json:"pressure,omitempty"`
	MajorAxisSize float64 `json:"majorAxisSize,omitempty"`
}

type streamInputMessage struct {
	Cmd        int             `json:"cmd"`
	InstanceID int32           `json:"instanceId"`
	MessageID  uint64          `json:"messageId"`
	Data       streamInputData `json:"data"`
}

func (h *viewerHub) sendInputLocked(cmd int, data streamInputData) error {
	h.inputSeq++
	return h.sendLocked(svcInput, streamInputMessage{Cmd: cmd, InstanceID: h.instance, MessageID: h.inputSeq, Data: data})
}

func (h *viewerHub) inputDevicesLocked(s *streamSession) error {
	return h.sendInputLocked(1, streamInputData{DisplayID: s.DisplayID, Ver: 1, Caps: 3})
}

func (h *viewerHub) inputLocked(s *streamSession, in viewerInput) error {
	if in.Type == "release" {
		return h.releaseInputLocked(s)
	}
	if s.DisplayID < 0 || s.State != "connected" {
		return errors.New("phone display is not ready for input")
	}
	data := streamInputData{DisplayID: s.DisplayID, X: in.X, Y: in.Y}
	if in.Type == "move" || in.Type == "button" || in.Type == "wheel" || in.Type == "touch" {
		if in.X < 0 || in.Y < 0 || in.X >= s.Width || in.Y >= s.Height {
			return errors.New("pointer is outside the phone display")
		}
	}
	var cmd int
	switch in.Type {
	case "key":
		if in.Key < 1 || in.Key > 32767 {
			return errors.New("invalid Android keycode")
		}
		cmd, data.Flags, data.Key, data.Down, data.MetaState = 3, 4096, in.Key, in.Down, in.MetaState
	case "move":
		cmd = 4
	case "button":
		if in.Key < 1 || in.Key > 3 {
			return errors.New("unsupported mouse button")
		}
		cmd, data.Key, data.Down = 5, in.Key, in.Down
	case "wheel":
		cmd, data.Key, data.Down = 5, 4, in.Down
	case "text":
		if len(in.Text) > 65536 {
			return errors.New("text input is too large")
		}
		if in.Text == "" {
			return nil
		}
		cmd, data.Text = 7, in.Text
	case "touch":
		if !s.Touch {
			return errors.New("phone display does not accept touch input")
		}
		if in.Event < 0 || in.Event > 3 || in.PointerID < 0 || in.PointerID > 31 ||
			in.Pressure < 0 || in.Pressure > 1 || in.MajorAxisSize < 0 {
			return errors.New("invalid touch event")
		}
		cmd, data.Event, data.PointerID, data.Pressure, data.MajorAxisSize = 9, in.Event, in.PointerID, in.Pressure, in.MajorAxisSize
	default:
		return fmt.Errorf("unknown input type %q", in.Type)
	}
	if err := h.sendInputLocked(cmd, data); err != nil {
		return err
	}
	switch in.Type {
	case "key":
		if in.Down {
			if s.keys == nil {
				s.keys = make(map[int]bool)
			}
			s.keys[in.Key] = true
		} else {
			delete(s.keys, in.Key)
		}
	case "button":
		if in.Down {
			if s.buttons == nil {
				s.buttons = make(map[int]viewerInput)
			}
			s.buttons[in.Key] = in
		} else {
			delete(s.buttons, in.Key)
		}
	case "touch":
		if in.Event == 1 || in.Event == 2 {
			if s.touches == nil {
				s.touches = make(map[int]viewerInput)
			}
			s.touches[in.PointerID] = in
		} else {
			delete(s.touches, in.PointerID)
		}
	}
	return nil
}

// Release before a deliberate stop/focus change. On a broken control socket the
// phone itself destroys these input devices; local tracking is discarded too.
func (h *viewerHub) releaseInputLocked(s *streamSession) error {
	var first error
	remember := func(err error) {
		if first == nil {
			first = err
		}
	}
	if s.DisplayID >= 0 {
		for key := range s.keys {
			remember(h.sendInputLocked(3, streamInputData{DisplayID: s.DisplayID, Flags: 4096, Key: key, Down: false}))
		}
		for key, in := range s.buttons {
			remember(h.sendInputLocked(5, streamInputData{DisplayID: s.DisplayID, Key: key, X: in.X, Y: in.Y, Down: false}))
		}
		for pointer, in := range s.touches {
			remember(h.sendInputLocked(9, streamInputData{DisplayID: s.DisplayID, PointerID: pointer, Event: 3, X: in.X, Y: in.Y}))
		}
	}
	clear(s.keys)
	clear(s.buttons)
	clear(s.touches)
	return first
}
