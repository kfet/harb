package reedercompat_test

// TestReederCompat wires the Reeder / GReader conformance suite from
// internal/reedercompat against the harb Server implementation.
//
// The contracts live in internal/reedercompat as a deliberately narrow,
// embedder-driven suite so that the harb server is exercised the
// same way a future OSS conformance kit would exercise any GReader-
// dialect server.

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/reader"
	"github.com/kfet/harb/internal/reedercompat"
	"github.com/kfet/harb/internal/store"
)

var testPwHash = mustHashPw()

func mustHashPw() string {
	h, err := auth.HashPassword("p")
	if err != nil {
		panic(err)
	}
	return h
}

// memOPML is a minimal in-memory OPMLProvider for the conformance run.
type memOPML struct {
	opml store.OPML
}

func (m *memOPML) Load() (*store.OPML, error) {
	cp := m.opml
	cp.Feeds = append([]store.Feed{}, m.opml.Feeds...)
	return &cp, nil
}
func (m *memOPML) Save(o *store.OPML) error {
	m.opml = *o
	m.opml.Feeds = append([]store.Feed{}, o.Feeds...)
	return nil
}
func (m *memOPML) Update(fn func(*store.OPML) error) error {
	cur, err := m.Load()
	if err != nil {
		return err
	}
	if err := fn(cur); err != nil {
		return err
	}
	return m.Save(cur)
}

func TestReederCompat(t *testing.T) {
	reedercompat.Run(t, func(t *testing.T) reedercompat.Harness {
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := auth.Config{Username: "u", PasswordHash: testPwHash}
		as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), cfg)
		tok, _ := as.IssueAPIToken("u", "p")
		op := &memOPML{}
		s := reader.New(st, as, op)
		// Crank MaxPage down so the continuation-paging contract is
		// exercised on a small seed without needing 1000 entries.
		s.MaxPage = 10
		handler := s.Routes(http.NewServeMux())

		seedTimes := func(t *testing.T, name, tag string, published, fetched []time.Time) (string, []string) {
			t.Helper()
			if len(published) != len(fetched) {
				t.Fatalf("seedTimes: published(%d) and fetched(%d) length mismatch", len(published), len(fetched))
			}
			u := "https://feed.example/" + name
			op.opml.Feeds = append(op.opml.Feeds, store.Feed{
				XMLURL: u, Title: name, Tags: []string{tag},
				HTMLURL: "https://feed.example",
			})
			fh := store.FeedHash(u)
			es := make([]store.Entry, len(published))
			for i := range published {
				es[i] = store.Entry{
					GUID:      name + "-g" + strconv.Itoa(i),
					Link:      "https://feed.example/" + name + "/" + strconv.Itoa(i),
					Title:     name + " " + strconv.Itoa(i),
					Content:   "c",
					Summary:   "s",
					Published: published[i],
					FetchedAt: fetched[i],
				}
			}
			if _, err := st.AppendEntries(fh, es); err != nil {
				t.Fatal(err)
			}
			got, err := st.ListEntries(fh)
			if err != nil {
				t.Fatal(err)
			}
			hashes := make([]string, len(got))
			byGUID := map[string]string{}
			for _, e := range got {
				byGUID[e.GUID] = e.Hash
			}
			for i := range es {
				hashes[i] = byGUID[es[i].GUID]
			}
			return u, hashes
		}
		seed := func(t *testing.T, name, tag string, count int) (string, []string) {
			t.Helper()
			now := time.Now().UTC()
			stamps := make([]time.Time, count)
			for i := range stamps {
				stamps[i] = now
			}
			return seedTimes(t, name, tag, stamps, stamps)
		}

		return reedercompat.Harness{
			Handler:       handler,
			Token:         tok,
			SeedFeed:      seed,
			SeedFeedTimes: seedTimes,
			SetRead: func(t *testing.T, hash string, v bool) time.Time {
				t.Helper()
				if err := st.SetRead(hash, v); err != nil {
					t.Fatal(err)
				}
				return st.EntryState(hash).UpdatedAt
			},
			SetStarred: func(t *testing.T, hash string, v bool) time.Time {
				t.Helper()
				if err := st.SetStarred(hash, v); err != nil {
					t.Fatal(err)
				}
				return st.EntryState(hash).UpdatedAt
			},
			SeedDriftTwin: func(t *testing.T, name, tag string) (string, string) {
				t.Helper()
				u := "https://feed.example/" + name
				op.opml.Feeds = append(op.opml.Feeds, store.Feed{
					XMLURL: u, Title: name, Tags: []string{tag},
					HTMLURL: "https://feed.example",
				})
				fh := store.FeedHash(u)
				guid := "https://feed.example/?p=12345"
				pub := time.Unix(1700000000, 0).UTC()
				mk := func(link string, fetched time.Time) store.Entry {
					// Empty Hash → AppendEntries computes EntryHash
					// (guid-only identity), mirroring the poll path.
					return store.Entry{
						GUID: guid, Link: link, Title: "Stable title",
						Content: "c", Summary: "s", Published: pub, FetchedAt: fetched,
					}
				}
				// First poll: /istorii/ link. Second poll: same guid,
				// /novini/ link (slug edited after publish).
				if _, err := st.AppendEntries(fh, []store.Entry{mk("https://feed.example/istorii/slug/", pub)}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.AppendEntries(fh, []store.Entry{mk("https://feed.example/novini/slug/", pub.Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
				return u, store.EntryHash(guid, "https://feed.example/istorii/slug/", "Stable title", pub)
			},
		}
	})
}
