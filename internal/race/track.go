package race

import (
	"strconv"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/track"
)

// The session's track (paddock#81): v1's Engine._apply_track, track_rank and the
// final verdict at the race end, from engine.py at v1.0.0. The detector
// (internal/track) gives its verdict frame by frame; the session keeps one, stable
// enough to speak and store, and publishes session.track each time that changes.

// trackRank is how good a verdict is: its confidence, then whether it names a
// layout (v1's track_rank).
func trackRank(m track.Match) (track.Confidence, bool) {
	return m.Confidence, m.TrackID != 0
}

// ranksBelow says a ranks below b, by trackRank.
func ranksBelow(a, b track.Match) bool {
	ac, an := trackRank(a)
	bc, bn := trackRank(b)
	if ac != bc {
		return ac < bc
	}
	return !an && bn
}

// sameDecision says a and b are the same decision: the same layout as surely. A
// change of candidates, fit or wording alone is not a change (paddock#81, R1).
func sameDecision(a, b track.Match) bool {
	aID, aConf := a.Decision()
	bID, bConf := b.Decision()
	return aID == bID && aConf == bConf
}

// applyTrack is v1's Engine._apply_track: keep the best verdict the session has
// had and never downgrade it on a blip. A layout the detector has ruled out is not
// a blip: the verdict was wrong. No layout at all is below a named one as unsure:
// at Spa (2026-10-18, those-who-run/those-who-run#275) an ambiguous pair gave way
// to an ambiguous nothing, and the session was saved without a track. Each change
// is a SessionTrackChanged on the frame s.
//
// One departure from v1, the maintainer's decision (paddock#81, 2026-10-30): "no
// layout fits" replaces a held verdict that names no layout. That verdict was only
// that several layouts near the start still fit, and the detector has since ruled
// every one out, so keeping it would say the track is still to be found when it
// is a track the table lacks or gets wrong. At Fuji on 2026-10-29 the session kept
// its pre-race "50 layouts near the start still fit" to the end, on v1 and on ecu.
// The detector's verdict is none only when nothing fits: Observe never gives its
// "no position data yet", and a verdict held at a session's open is never none.
func (m *Machine) applyTrack(match track.Match, s *state, t time.Time) {
	sess := m.session
	if held := sess.track; held != nil {
		noneFits := match.Confidence == track.None && held.TrackID == 0
		if ranksBelow(match, *held) && !noneFits && (held.TrackID == 0 || m.tracks.IsAlive(held.TrackID)) {
			return
		}
		if sameDecision(match, *held) {
			return
		}
	}
	sess.track, sess.carried = &match, false
	sess.summary.Track = verdict(match)
	m.trackChanges = append(m.trackChanges, sess.track)
	seq := strconv.FormatUint(uint64(s.seq), 10)
	out := Output{
		Event:   m.event(sess.id+":session.track:"+m.run+":"+seq, t),
		Session: sess.id,
		Track: &eventsv1.SessionTrackChanged{
			SessionId: sess.id, Track: verdict(match), At: s.seq, RunId: m.run,
		},
	}
	// Two changes on one frame (the verdict held as the session opens, then the
	// line crossed on that same frame) would share an id, and EVENTS would drop
	// the second as a duplicate: the frame publishes one, its last verdict. v1
	// logged both.
	for i, o := range m.out {
		if o.Track != nil && o.Session == sess.id && o.Track.GetAt() == s.seq {
			m.out[i] = out
			return
		}
	}
	m.out = append(m.out, out)
}

// finalTrack is v1's track at the race end: the detector's final candidates and
// fit replace the session's verdict when they are the same decision, and the same
// circuit, so a carried circuit stays (paddock#81, R5).
func (m *Machine) finalTrack() {
	final := m.tracks.Match()
	if held := m.session.track; held != nil && sameDecision(final, *held) && final.Family == held.Family {
		m.session.track, m.session.carried = &final, false
		m.session.summary.Track = verdict(final)
	}
}

// seeTrack gives the detector a frame the machine does not step: a held start
// judged a replay or a demo, the frames ignored after one, and the frames ignored
// after a session ended paused. No session is open on them, so the verdict is
// no one's; the detector needs them for the menus and loading screens that start
// it over, and for where the car is when the next start comes. A departure from
// v1.0.0, whose Engine fed the detector only in _step: at Road Atlanta on
// 2026-10-30 it last saw a GT7 demo's few frames at another circuit, missed the
// 22,000 frames of menus, loading and the formation lap after it, took the first
// tick for a teleport, so settled no line with it, and then counted the path
// across Sardegna - Road Track - A's line, where the two tables' coordinates meet:
// "probable Sardegna" until a full lap confirmed Road Atlanta.
func (m *Machine) seeTrack(s *state) {
	m.tracks.Observe(s.frame)
}

// confidences are the contracts' TrackConfidence for each of the detector's.
var confidences = map[track.Confidence]eventsv1.TrackConfidence{
	track.None:      eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE,
	track.Ambiguous: eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS,
	track.Probable:  eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE,
	track.Confirmed: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED,
}

// verdict is the match as the contracts' TrackVerdict, as v1's track_record
// stores it, with at most track.MaxReportedCandidates candidates, the best: the
// detector keeps v1's whole list, and the cap is applied here, where it is
// published (the maintainer's decision on ecu#17). Fit is rounded to 3 decimals,
// as v1 rounds it.
//
// Carried is false: continue.go sets it on the verdict a session continued after
// an ecu restart starts with (paddock#76).
func verdict(m track.Match) *eventsv1.TrackVerdict {
	v := &eventsv1.TrackVerdict{
		Name:       m.Name,
		Confidence: confidences[m.Confidence],
		Family:     m.Family,
		Evidence:   m.Evidence,
	}
	if m.TrackID != 0 {
		id := m.TrackID
		v.TrackId = &id
	}
	cands := m.Candidates[:min(len(m.Candidates), track.MaxReportedCandidates)]
	v.Candidates = make([]*eventsv1.TrackCandidate, len(cands))
	for i, c := range cands {
		v.Candidates[i] = &eventsv1.TrackCandidate{
			TrackId: c.Track.ID, Name: c.Track.Name, Fit: round3(c.Fit), LineCrossed: c.LineCrossed,
		}
	}
	return v
}
