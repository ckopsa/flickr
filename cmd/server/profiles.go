package main

// Profiles: a face, an audience, and what a kid profile is shown.
//
// A profile used to be a name — the string every playback row is keyed by.
// It carries two more facts now: an AVATAR, so the gate is a row of faces
// rather than a row of words, and KID, which is not decoration. A kid
// profile's library, search, resume shelf and routes are the same documents
// with the titles it may not see left out, and an address aimed straight at
// one of them — a play, an item, a work, a read — is refused with a problem
// that names the way back.
//
// The rating read is the certification the TMDB enrichment carries
// (flickr-duw.7). Nothing new is fetched or stored for the filter: what the
// library already knows about a title is what decides it.

import (
	"fmt"
	"net/http"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/store"
	"flickr/internal/works"
)

// profileAvatars are the faces a new profile is given when it names none.
// The same name always draws the same one, so a household's gate keeps the
// faces it had rather than reshuffling them on every create.
var profileAvatars = []string{"🦊", "🐼", "🐙", "🦉", "🐝", "🐳", "🦄", "🐢", "🦁", "🐸", "🦖", "🐧"}

func defaultAvatar(name string) string {
	sum := 0
	for _, r := range name {
		sum += int(r)
	}
	return profileAvatars[sum%len(profileAvatars)]
}

// kidCertifications is every US rating a kid profile may see. PG and TV-PG
// are the ceiling, and the ratings under them come with it. Everything else
// is above it — PG-13, R, TV-14, TV-MA — and so is a title NOBODY has rated:
// an unrated title is an unknown one, and a child's shelf is the wrong place
// to find out what it is.
var kidCertifications = map[string]bool{
	"G": true, "PG": true,
	"TV-Y": true, "TV-Y7": true, "TV-Y7-FV": true, "TV-G": true, "TV-PG": true,
}

func fitForKids(cert string) bool {
	return kidCertifications[strings.ToUpper(strings.TrimSpace(cert))]
}

// workCertification is the rating of the TITLE, which is the representative
// member's — a show's episodes all carry the show's, and that is the one a
// shelf filters on.
func workCertification(wk *works.Work) string {
	if rep := memberByID(wk, wk.RepresentativeItemID); rep != nil && rep.Enrichment != nil {
		return rep.Enrichment.Certification
	}
	return ""
}

// kidWorks is the filter itself: pure, so the table test drives it directly
// and every document that calls worksFor inherits one rule rather than
// keeping its own copy.
func kidWorks(ws []works.Work) []works.Work {
	out := make([]works.Work, 0, len(ws))
	for i := range ws {
		if fitForKids(workCertification(&ws[i])) {
			out = append(out, ws[i])
		}
	}
	return out
}

// kidProfile is whether the profile asking is a child's. An unknown name is
// not — profiles are made at the gate, and a client_id nobody created is a
// stranger, not a kid.
func (s *server) kidProfile(name string) bool {
	if name == "" {
		return false
	}
	u, err := s.state.User(name)
	return err == nil && u != nil && u.Kid
}

// worksFor is the library AS THIS PROFILE SEES IT: the whole projection for
// everybody else, and the titles fit for a kid for a kid. The library,
// search, continue and route documents all read through it, so the four
// agree without any of them knowing what a certification is.
func (s *server) worksFor(r *http.Request) ([]works.Work, error) {
	ws, err := s.buildWorks()
	if err != nil || !s.kidProfile(profileOf(r)) {
		return ws, err
	}
	return kidWorks(ws), nil
}

// playProfile is who is playing: the client_id in the play body, which is
// what the player sends, or the profile the request itself carries.
func playProfile(r *http.Request, clientID string) string {
	if c := strings.TrimSpace(clientID); c != "" {
		return c
	}
	return profileOf(r)
}

// refuseItem is the other half of the filter: the shelves leave the title
// out, and an address aimed straight at it — a stale tab, a pasted link, a
// cast device replaying an old session — is told why, and where to go. The
// play, the item document and the read all ask it, so a kid who types an
// address is turned away in the same words wherever they type it.
// nil when this profile may have it, which is every profile but a kid's.
func (s *server) refuseItem(profile string, it store.Item) *hyper.Problem {
	if !s.kidProfile(profile) {
		return nil
	}
	cert := ""
	if it.Enrichment != nil {
		cert = it.Enrichment.Certification
	}
	if cert == "" {
		// An unenriched member of an enriched title — a featurette beside
		// its film — is rated by its work rather than by its own blank.
		if ws, err := s.buildWorks(); err == nil {
			if wk := works.ByItem(ws)[it.ID]; wk != nil {
				cert = workCertification(wk)
			}
		}
	}
	return notForThisProfile(profile, itemTitle(it), cert)
}

// refuseWork is the same refusal for a whole title: the work document is a
// shelf of its own, and one the grid left out is not opened by typing its
// key. A work is rated by its representative member, the way the grid's
// filter rates it.
func (s *server) refuseWork(profile string, wk *works.Work) *hyper.Problem {
	if !s.kidProfile(profile) {
		return nil
	}
	return notForThisProfile(profile, wk.Title, workCertification(wk))
}

// notForThisProfile is the refusal itself, and nil when the rating passes:
// what was asked for, how it is rated (or that nobody rated it, which is the
// same answer), and the gate as the way on.
func notForThisProfile(profile, title, cert string) *hyper.Problem {
	if fitForKids(cert) {
		return nil
	}
	rated := "it is not rated at all"
	if cert != "" {
		rated = "it is rated " + cert
	}
	p := hyper.Refuse(http.StatusForbidden, "not-for-this-profile",
		"Not for this profile",
		fmt.Sprintf("%q is a kids profile, and %s: %s", profile, title, rated)).
		WithRemedy("switch to another profile — the chip at the top of the page opens the gate",
			&hyper.Link{Href: "/api/users", Title: "Profiles"})
	return &p
}
