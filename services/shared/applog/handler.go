// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// prefixHandler writes log records as:
//
//	HH:MM:SS.sss [procname] LEVEL msg key=value key=value
//
// This keeps stderr human-readable in the UI debug panel while still being
// grep-friendly for post-mortem analysis.
type prefixHandler struct {
	w      io.Writer
	name   string
	level  *slog.LevelVar
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

func newPrefixHandler(w io.Writer, name string, level *slog.LevelVar) *prefixHandler {
	return &prefixHandler{
		w:     w,
		name:  name,
		level: level,
		mu:    new(sync.Mutex),
	}
}

func (h *prefixHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *prefixHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.Grow(128)

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	b.WriteString(ts.Format("15:04:05.000"))
	b.WriteString(" [")
	b.WriteString(h.name)
	b.WriteString("] ")
	b.WriteString(levelTag(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)

	for _, a := range h.attrs {
		appendAttr(&b, h.groups, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *prefixHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &nh
}

func (h *prefixHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.groups = append(append([]string(nil), h.groups...), name)
	return &nh
}

func levelTag(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

func appendAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		grp := a.Value.Group()
		if len(grp) == 0 {
			return
		}
		ng := groups
		if a.Key != "" {
			ng = append(append([]string(nil), groups...), a.Key)
		}
		for _, ga := range grp {
			appendAttr(b, ng, ga)
		}
		return
	}
	b.WriteByte(' ')
	if len(groups) > 0 {
		b.WriteString(strings.Join(groups, "."))
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(formatValue(a.Value))
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if needsQuote(s) {
			return strconv.Quote(s)
		}
		return s
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	default:
		s := fmt.Sprint(v.Any())
		if needsQuote(s) {
			return strconv.Quote(s)
		}
		return s
	}
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' {
			return true
		}
	}
	return false
}
