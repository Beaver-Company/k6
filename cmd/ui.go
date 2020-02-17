/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2019 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// go:generate enumer -type=UIMode -transform=snake -trimprefix=UIMode -output ui_mode_gen.go

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/sirupsen/logrus"

	"github.com/loadimpact/k6/core/local"
	"github.com/loadimpact/k6/lib"
	"github.com/loadimpact/k6/ui/pb"
)

// UIMode defines various rendering methods
type UIMode uint8

//nolint: golint
const (
	// max length of left-side progress bar text before trimming is forced
	maxLeftLength           = 30
	UIModeResponsive UIMode = iota + 1
	UIModeCompact
	UIModeFull
)

// Decode implements envconfig.Decoder
func (i *UIMode) Decode(value string) (err error) {
	*i, err = UIModeString(value)
	return
}

// A writer that syncs writes with a mutex and, if the output is a TTY, clears before newlines.
type consoleWriter struct {
	Writer io.Writer
	IsTTY  bool
	Mutex  *sync.Mutex

	// Used for flicker-free persistent objects like the progressbars
	PersistentText func()
}

func (w *consoleWriter) Write(p []byte) (n int, err error) {
	origLen := len(p)
	if w.IsTTY {
		// Add a TTY code to erase till the end of line with each new line
		// TODO: check how cross-platform this is...
		p = bytes.Replace(p, []byte{'\n'}, []byte{'\x1b', '[', '0', 'K', '\n'}, -1)
	}

	w.Mutex.Lock()
	n, err = w.Writer.Write(p)
	if w.PersistentText != nil {
		w.PersistentText()
	}
	w.Mutex.Unlock()

	if err != nil && n < origLen {
		return n, err
	}
	return origLen, err
}

func printBar(bar *pb.ProgressBar, rightText string) {
	end := "\n"
	if stdout.IsTTY {
		// If we're in a TTY, instead of printing the bar and going to the next
		// line, erase everything till the end of the line and return to the
		// start, so that the next print will overwrite the same line.
		//
		// TODO: check for cross platform support
		end = "\x1b[0K\r"
	}
	rendered := bar.Render(0, 0)
	// Only output the left and middle part of the progress bar
	fprintf(stdout, "%s %s %s%s", rendered.Left, rendered.Progress(), rightText, end)
}

func renderMultipleBars(
	isTTY, goBack bool, maxLeft, widthDelta int, pbs []*pb.ProgressBar,
) (string, int) {
	lineEnd := "\n"
	if isTTY {
		//TODO: check for cross platform support
		lineEnd = "\x1b[K\n" // erase till end of line
	}

	var (
		longestLine int
		// Maximum length of each right side column except last,
		// used to calculate the padding between columns.
		maxRColumnLen = make([]int, 2)
		pbsCount      = len(pbs)
		rendered      = make([]pb.ProgressBarRender, pbsCount)
		result        = make([]string, pbsCount+2)
	)

	result[0] = lineEnd // start with an empty line

	// First pass to render all progressbars and get the maximum
	// lengths of right-side columns.
	for i, pb := range pbs {
		rend := pb.Render(maxLeft, widthDelta)
		for i := range rend.Right {
			// Skip last column, since there's nothing to align after it (yet?).
			if i == len(rend.Right)-1 {
				break
			}
			if len(rend.Right[i]) > maxRColumnLen[i] {
				maxRColumnLen[i] = len(rend.Right[i])
			}
		}
		rendered[i] = rend
	}

	// Second pass to render final output, applying padding where needed
	for i := range rendered {
		rend := rendered[i]
		if rend.Hijack != "" {
			result[i+1] = rend.Hijack + lineEnd
			continue
		}
		var leftText, rightText string
		leftPadFmt := fmt.Sprintf("%%-%ds", maxLeft)
		leftText = fmt.Sprintf(leftPadFmt, rend.Left)
		for i := range rend.Right {
			rpad := 0
			if len(maxRColumnLen) > i {
				rpad = maxRColumnLen[i]
			}
			rightPadFmt := fmt.Sprintf(" %%-%ds", rpad+1)
			rightText += fmt.Sprintf(rightPadFmt, rend.Right[i])
		}
		// Get visible line length, without ANSI escape sequences (color)
		status := fmt.Sprintf(" %s ", rend.Status())
		line := leftText + status + rend.Progress() + rightText
		lineRuneLen := utf8.RuneCountInString(line)
		if lineRuneLen > longestLine {
			longestLine = lineRuneLen
		}
		if !noColor {
			rend.Color = true
			status = fmt.Sprintf(" %s ", rend.Status())
			line = fmt.Sprintf(leftPadFmt+"%s%s%s",
				rend.Left, status, rend.Progress(), rightText)
		}
		result[i+1] = line + lineEnd
	}

	if isTTY && goBack {
		// Go back to the beginning
		//TODO: check for cross platform support
		result[pbsCount+1] = fmt.Sprintf("\r\x1b[%dA", pbsCount+1)
	} else {
		result[pbsCount+1] = "\n"
	}

	return strings.Join(result, ""), longestLine
}

//TODO: show other information here?
//TODO: add a no-progress option that will disable these
//TODO: don't use global variables...
// nolint:funlen
func showProgress(
	ctx context.Context, conf Config,
	execScheduler *local.ExecutionScheduler, logger *logrus.Logger,
) {
	if quiet || conf.HTTPDebug.Valid && conf.HTTPDebug.String != "" {
		return
	}

	pbs := []*pb.ProgressBar{execScheduler.GetInitProgressBar()}
	for _, s := range execScheduler.GetExecutors() {
		pbs = append(pbs, s.GetProgress())
	}

	termWidth, _, err := terminal.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		logger.WithError(err).Warn("error getting terminal size")
		termWidth = 80 // TODO: something safer, return error?
	}

	// Get the longest left side string length, to align progress bars
	// horizontally and trim excess text.
	var leftLen int64
	for _, pb := range pbs {
		l := pb.Left()
		leftLen = lib.Max(int64(len(l)), leftLen)
	}
	// Limit to maximum left text length
	maxLeft := int(lib.Min(leftLen, maxLeftLength))

	var widthDelta int
	var progressBarsLastRender []byte
	// default responsive render
	renderProgressBars := func(goBack bool) {
		barText, longestLine := renderMultipleBars(stdoutTTY, goBack, maxLeft, widthDelta, pbs)
		// -1 to allow some "breathing room" near the edge
		widthDelta = termWidth - longestLine - 1
		progressBarsLastRender = []byte(barText)
	}

	if conf.UIMode.String == UIModeCompact.String() {
		widthDelta = -pb.DefaultWidth
	}

	if conf.UIMode.String != UIModeResponsive.String() {
		renderProgressBars = func(goBack bool) {
			barText, _ := renderMultipleBars(stdoutTTY, goBack, maxLeft, widthDelta, pbs)
			progressBarsLastRender = []byte(barText)
		}
	}

	printProgressBars := func() {
		_, _ = stdout.Writer.Write(progressBarsLastRender)
	}

	//TODO: make configurable?
	updateFreq := 1 * time.Second
	//TODO: remove !noColor after we fix how we handle colors (see the related
	//description in the TODO message in cmd/root.go)
	if stdoutTTY && !noColor {
		updateFreq = 100 * time.Millisecond
	}

	ctxDone := ctx.Done()
	ticker := time.NewTicker(updateFreq)
	sigwinch := NotifyWindowResize()
	fd := int(os.Stdout.Fd())
	for {
		select {
		case <-ctxDone:
			// FIXME: Remove this...
			// Add a small delay to allow executors time to process
			// the done context, so that the correct status symbol is
			// outputted for each progress bar.
			time.Sleep(50 * time.Millisecond)
			renderProgressBars(false)
			printProgressBars()
			return
		case <-ticker.C:
			// Optional "polling" method, platform dependent.
			termWidth, _, _ = GetTermSize(fd, termWidth)
		case <-sigwinch:
			// More efficient SIGWINCH method on *nix.
			termWidth, _, _ = terminal.GetSize(fd)
		}
		renderProgressBars(true)
		outMutex.Lock()
		printProgressBars()
		outMutex.Unlock()
	}
}
