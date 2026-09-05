// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// tick is one turn of the player's loop: the wall clock moves on and the
// clock is read with however much the device has played by then.
//
// These tests run in a synctest bubble, where the time package's clock is
// the bubble's own and moves only once nothing is left to run. Sleeping
// through a second of ticks costs no time at all and lands exactly on the
// second, so the clock — which is a wall clock steered by the sound — can
// be driven over real durations without the test taking any time or
// depending on how busy the machine is.
func tick(c *clock, played float64) float64 {
	time.Sleep(time.Second / 60)
	now, _ := c.now(played)
	return now
}

// TestClockFollowsTheSound checks the ordinary case: the device plays on,
// the disc's own time line runs with it, and the clock reports it.
func TestClockFollowsTheSound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		if _, ok := c.now(0); ok {
			t.Error("the clock reported a time before any sound placed it")
		}

		c.add(0, 100) // at the start of the device, the disc is 100s in
		var played, now float64
		for range 200 {
			played += 1.0 / 60
			now = tick(&c, played)
		}

		if want := 100 + played; math.Abs(now-want) > 0.05 {
			t.Errorf("clock = %.3f, want about %.3f", now, want)
		}
	})
}

// TestClockOutrunsNothing is the heart of it. The device's reported
// position is a derived, smoothed figure, and where it steps forward the
// picture must not be dragged along with it: a clock that simply reported
// the sound's position would leap, every queued picture would fall due at
// once, and the video would tear past at whatever rate it decodes.
func TestClockOutrunsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 0)

		// A second of ordinary playing, then the device claims to have
		// stepped ten seconds on between one tick and the next.
		var played, before float64
		for range 60 {
			played += 1.0 / 60
			before = tick(&c, played)
		}

		played += 10
		after := tick(&c, played)

		if step := after - before; step > 1.1 {
			t.Errorf("the clock moved %.2fs in one tick: the picture would run away", step)
		}
	})
}

// TestClockCatchesUp checks the other half of easing: what it will not do
// in one step it still has to do. A clock that shrugged off a step in the
// device's position and never came back would leave the picture that much
// out from the sound for good.
func TestClockCatchesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 0)

		var played float64
		for range 60 {
			played += 1.0 / 60
			tick(&c, played)
		}

		// The device's position steps forward by ten seconds, and then
		// two seconds of ordinary playing to take it in. Easing a
		// twentieth of the error out per tick leaves a fiftieth of it
		// after the first second, well clear of the tolerance.
		played += 10
		var now float64
		for range 120 {
			played += 1.0 / 60
			now = tick(&c, played)
		}

		if math.Abs(now-played) > 0.1 {
			t.Errorf("clock = %.2f two seconds after the device stepped past %.2f, "+
				"want it to have caught up", now, played)
		}
	})
}

// TestClockSnapsOverAJump checks that a disc jumping somewhere else is not
// eased into. The sound after a jump belongs to another part of the disc
// entirely, so there is nothing to ease towards and the clock goes
// straight there.
func TestClockSnapsOverAJump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 0)

		var played float64
		for range 60 {
			played += 1.0 / 60
			tick(&c, played)
		}

		// The disc jumps: the next sound is from 900 seconds in.
		c.add(played, 900)
		played += 1.0 / 60
		now := tick(&c, played)

		if math.Abs(now-900) > 0.1 {
			t.Errorf("clock = %.2f after a jump to 900s, want it to go straight there", now)
		}
	})
}

// TestClockHoldsThroughAPause checks that pausing does not move the disc
// on. The device stops playing while the viewer is away, so its position
// stands still; if the wall clock ran on regardless, the clock would come
// back that much further down the disc and every picture queued behind
// that time would be dropped to catch up.
func TestClockHoldsThroughAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 0)

		var played, before float64
		for range 60 {
			played += 1.0 / 60
			before = tick(&c, played)
		}

		c.pause()
		time.Sleep(5 * time.Second) // the viewer is away
		c.resume()

		// The device played nothing while it was stopped, so it comes
		// back at the position it went away on.
		after := tick(&c, played)

		if step := after - before; math.Abs(step) > 0.05 {
			t.Errorf("the clock moved %.3fs across a 5s pause: the picture would jump", step)
		}
	})
}

// TestClockKeepsTimeAfterAPause checks that the pause leaves the clock
// running as it was: it must still follow the sound afterwards, at one
// second per second, rather than come back frozen or offset.
func TestClockKeepsTimeAfterAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 100) // the disc is 100s in at the start of the device

		var played float64
		for range 60 {
			played += 1.0 / 60
			tick(&c, played)
		}

		c.pause()
		time.Sleep(5 * time.Second)
		c.resume()

		// Two more seconds of ordinary playing.
		var now float64
		for range 120 {
			played += 1.0 / 60
			now = tick(&c, played)
		}

		if want := 100 + played; math.Abs(now-want) > 0.05 {
			t.Errorf("clock = %.3f after a pause, want about %.3f: the pause left it adrift",
				now, want)
		}
	})
}

// TestClockPauseIsIdempotent checks the toggle cannot be knocked out of
// step: a pause while paused, or a resume while running, must not move the
// clock.
func TestClockPauseIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c clock
		c.add(0, 0)

		var played, before float64
		for range 60 {
			played += 1.0 / 60
			before = tick(&c, played)
		}

		c.resume() // never paused
		if now, _ := c.now(played); math.Abs(now-before) > 0.05 {
			t.Errorf("resuming a running clock moved it to %.3f, want about %.3f", now, before)
		}

		c.pause()
		time.Sleep(time.Second)
		c.pause() // already paused: must not take the second reading
		time.Sleep(time.Second)
		c.resume()

		if after := tick(&c, played); math.Abs(after-before) > 0.05 {
			t.Errorf("the clock moved %.3fs across a doubled pause", after-before)
		}
	})
}
