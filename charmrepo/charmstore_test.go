// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmrepo_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	jujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/charmstore.v4"
	"gopkg.in/juju/charmstore.v4/csclient"
	"gopkg.in/juju/charmstore.v4/params"
	"gopkg.in/mgo.v2"

	"gopkg.in/juju/charm.v5-unstable"
	"gopkg.in/juju/charm.v5-unstable/charmrepo"
	charmtesting "gopkg.in/juju/charm.v5-unstable/testing"
)

type charmStoreSuite struct {
	jujutesting.IsolationSuite
}

var _ = gc.Suite(&charmStoreSuite{})

func (s *charmStoreSuite) TestURL(c *gc.C) {
	repo := charmrepo.NewCharmStore(charmrepo.NewCharmStoreParams{
		URL: "https://1.2.3.4/charmstore",
	})
	c.Assert(repo.(*charmrepo.CharmStore).URL(), gc.Equals, "https://1.2.3.4/charmstore")
}

func (s *charmStoreSuite) TestDefaultURL(c *gc.C) {
	repo := charmrepo.NewCharmStore(charmrepo.NewCharmStoreParams{})
	c.Assert(repo.(*charmrepo.CharmStore).URL(), gc.Equals, csclient.ServerURL)
}

var serverParams = charmstore.ServerParams{
	AuthUsername: "test-user",
	AuthPassword: "test-password",
}

type charmStoreBaseSuite struct {
	charmtesting.IsolatedMgoSuite
	srv  *httptest.Server
	repo charmrepo.Interface
}

var _ = gc.Suite(&charmStoreBaseSuite{})

func (s *charmStoreBaseSuite) SetUpTest(c *gc.C) {
	s.IsolatedMgoSuite.SetUpTest(c)
	s.srv = newServer(c, s.Session)
	s.repo = charmrepo.NewCharmStore(charmrepo.NewCharmStoreParams{
		URL: s.srv.URL,
	})
	s.PatchValue(&charmrepo.CacheDir, c.MkDir())
}

func (s *charmStoreBaseSuite) TearDownTest(c *gc.C) {
	s.srv.Close()
	s.IsolatedMgoSuite.TearDownTest(c)
}

// addCharm uploads a charm to the testing charm store, and returns the
// resulting charm and charm URL.
func (s *charmStoreBaseSuite) addCharm(c *gc.C, url, name string) (charm.Charm, *charm.URL) {
	client := csclient.New(csclient.Params{
		URL:      s.srv.URL,
		User:     serverParams.AuthUsername,
		Password: serverParams.AuthPassword,
	})
	ch := TestCharms.CharmDir(name)
	id, err := client.UploadCharm(charm.MustParseReference(url), ch)
	c.Assert(err, jc.ErrorIsNil)
	return ch, (*charm.URL)(id)
}

type charmStoreRepoSuite struct {
	charmStoreBaseSuite
}

var _ = gc.Suite(&charmStoreRepoSuite{})

func (s *charmStoreRepoSuite) TestGet(c *gc.C) {
	expect, url := s.addCharm(c, "~who/trusty/mysql", "mysql")
	ch, err := s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	checkCharm(c, ch, expect)
}

func (s *charmStoreRepoSuite) TestGetRevisions(c *gc.C) {
	// Use different charms so that revision is actually increased.
	s.addCharm(c, "~dalek/trusty/riak", "wordpress")
	expect1, url1 := s.addCharm(c, "~dalek/trusty/riak", "riak")
	expect2, _ := s.addCharm(c, "~dalek/trusty/riak", "wordpress")

	// Retrieve an old revision.
	ch, err := s.repo.Get(url1)
	c.Assert(err, jc.ErrorIsNil)
	checkCharm(c, ch, expect1)

	// Retrieve the latest revision.
	ch, err = s.repo.Get(charm.MustParseURL("cs:~dalek/trusty/riak"))
	c.Assert(err, jc.ErrorIsNil)
	checkCharm(c, ch, expect2)
}

func (s *charmStoreRepoSuite) TestGetCache(c *gc.C) {
	_, url := s.addCharm(c, "~who/trusty/mysql", "mysql")
	ch, err := s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	path := ch.(*charm.CharmArchive).Path
	c.Assert(hashOfPath(c, path), gc.Equals, hashOfCharm(c, "mysql"))
}

func (s *charmStoreRepoSuite) TestGetSameCharm(c *gc.C) {
	_, url := s.addCharm(c, "~who/precise/wordpress", "wordpress")
	getModTime := func(path string) time.Time {
		info, err := os.Stat(path)
		c.Assert(err, jc.ErrorIsNil)
		return info.ModTime()
	}

	// Retrieve a charm.
	ch1, err := s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)

	// Retrieve its cache file modification time.
	path := ch1.(*charm.CharmArchive).Path
	modTime := getModTime(path)

	// Retrieve the same charm again.
	ch2, err := s.repo.Get(url.WithRevision(-1))
	c.Assert(err, jc.ErrorIsNil)

	// Check this is the same charm, and its underlying cache file is the same.
	checkCharm(c, ch2, ch1)
	c.Assert(ch2.(*charm.CharmArchive).Path, gc.Equals, path)

	// Check the same file has been reused.
	c.Assert(modTime.Equal(getModTime(path)), jc.IsTrue)
}

func (s *charmStoreRepoSuite) TestGetInvalidCache(c *gc.C) {
	_, url := s.addCharm(c, "~who/trusty/mysql", "mysql")

	// Retrieve a charm.
	ch1, err := s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)

	// Modify its cache file to make it invalid.
	path := ch1.(*charm.CharmArchive).Path
	err = ioutil.WriteFile(path, []byte("invalid"), 0644)
	c.Assert(err, jc.ErrorIsNil)

	// Retrieve the same charm again.
	_, err = s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)

	// Check that the cache file have been properly rewritten.
	c.Assert(hashOfPath(c, path), gc.Equals, hashOfCharm(c, "mysql"))
}

func (s *charmStoreRepoSuite) TestGetIncreaseStats(c *gc.C) {
	_, url := s.addCharm(c, "~who/precise/wordpress", "wordpress")

	// Retrieve the charm.
	_, err := s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	s.checkCharmDownloads(c, url, 1)

	// Retrieve the charm again.
	_, err = s.repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	s.checkCharmDownloads(c, url, 2)
}

func (s *charmStoreRepoSuite) TestGetWithTestMode(c *gc.C) {
	_, url := s.addCharm(c, "~who/precise/wordpress", "wordpress")

	// Use a repo with test mode enabled to download a charm a couple of
	// times, and check the downloads count is not increased.
	repo := s.repo.(*charmrepo.CharmStore).WithTestMode()
	_, err := repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	_, err = repo.Get(url)
	c.Assert(err, jc.ErrorIsNil)
	s.checkCharmDownloads(c, url, 0)
}

func (s *charmStoreRepoSuite) TestGetErrorBundle(c *gc.C) {
	ch, err := s.repo.Get(charm.MustParseURL("cs:bundle/django"))
	c.Assert(err, gc.ErrorMatches, `expected a charm URL, got bundle URL "cs:bundle/django"`)
	c.Assert(ch, gc.IsNil)
}

func (s *charmStoreRepoSuite) TestGetErrorCacheDir(c *gc.C) {
	parentDir := c.MkDir()
	err := os.Chmod(parentDir, 0)
	c.Assert(err, jc.ErrorIsNil)
	defer os.Chmod(parentDir, 0755)
	s.PatchValue(&charmrepo.CacheDir, filepath.Join(parentDir, "cache"))

	ch, err := s.repo.Get(charm.MustParseURL("cs:trusty/django"))
	c.Assert(err, gc.ErrorMatches, `cannot create the cache directory: .*: permission denied`)
	c.Assert(ch, gc.IsNil)
}

func (s *charmStoreRepoSuite) TestGetErrorCharmNotFound(c *gc.C) {
	ch, err := s.repo.Get(charm.MustParseURL("cs:trusty/no-such"))
	c.Assert(err, gc.ErrorMatches, `cannot retrieve charm "cs:trusty/no-such": charm not found`)
	c.Assert(ch, gc.IsNil)
}

func (s *charmStoreRepoSuite) TestGetErrorServer(c *gc.C) {
	// Set up a server always returning errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"Message": "bad wolf", "Code": "bad request"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	// Try getting a charm from the server.
	repo := charmrepo.NewCharmStore(charmrepo.NewCharmStoreParams{
		URL: srv.URL,
	})
	ch, err := repo.Get(charm.MustParseURL("cs:trusty/django"))
	c.Assert(err, gc.ErrorMatches, `cannot retrieve charm "cs:trusty/django": cannot get archive: bad wolf`)
	c.Assert(ch, gc.IsNil)
}

func (s *charmStoreRepoSuite) TestGetErrorHashMismatch(c *gc.C) {
	_, url := s.addCharm(c, "~dalek/trusty/riak", "riak")

	// Set up a proxy server that modifies the returned hash.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db := s.Session.DB("charm-testing")
		handler, err := charmstore.NewServer(db, nil, "", serverParams, charmstore.V4)
		c.Assert(err, jc.ErrorIsNil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		w.Header().Set(params.EntityIdHeader, rec.Header().Get(params.EntityIdHeader))
		w.Header().Set(params.ContentHashHeader, "invalid")
		w.Write(rec.Body.Bytes())
	}))
	defer srv.Close()

	// Try getting a charm from the server.
	repo := charmrepo.NewCharmStore(charmrepo.NewCharmStoreParams{
		URL: srv.URL,
	})
	ch, err := repo.Get(url)
	c.Assert(err, gc.ErrorMatches, `hash mismatch; network corruption\?`)
	c.Assert(ch, gc.IsNil)
}

func (s *charmStoreRepoSuite) TestLatest(c *gc.C) {
	// Add some charms to the charm store.
	s.addCharm(c, "~who/trusty/mysql", "mysql")
	s.addCharm(c, "~who/precise/wordpress", "wordpress")
	// Use different charms so that revision is actually increased.
	s.addCharm(c, "~dalek/trusty/riak", "wordpress")
	s.addCharm(c, "~dalek/trusty/riak", "riak")
	s.addCharm(c, "~dalek/trusty/riak", "wordpress")
	s.addCharm(c, "~dalek/trusty/riak", "riak")

	// Calculate and store the expected hashes for re uploaded charms.
	mysqlHash := hashOfCharm(c, "mysql")
	wordpressHash := hashOfCharm(c, "wordpress")
	riakHash := hashOfCharm(c, "riak")

	// Define the tests to be run.
	tests := []struct {
		about string
		urls  []*charm.URL
		revs  []charmrepo.CharmRevision
	}{{
		about: "no urls",
	}, {
		about: "charm not found",
		urls:  []*charm.URL{charm.MustParseURL("cs:trusty/no-such-42")},
		revs: []charmrepo.CharmRevision{{
			Err: charmrepo.CharmNotFound("cs:trusty/no-such"),
		}},
	}, {
		about: "resolve",
		urls: []*charm.URL{
			charm.MustParseURL("cs:~who/trusty/mysql-42"),
			charm.MustParseURL("cs:~who/trusty/mysql-0"),
			charm.MustParseURL("cs:~who/trusty/mysql"),
		},
		revs: []charmrepo.CharmRevision{{
			Revision: 0,
			Sha256:   mysqlHash,
		}, {
			Revision: 0,
			Sha256:   mysqlHash,
		}, {
			Revision: 0,
			Sha256:   mysqlHash,
		}},
	}, {
		about: "multiple charms",
		urls: []*charm.URL{
			charm.MustParseURL("cs:~who/precise/wordpress"),
			charm.MustParseURL("cs:~who/trusty/mysql-47"),
			charm.MustParseURL("cs:~dalek/trusty/no-such"),
			charm.MustParseURL("cs:~dalek/trusty/riak-0"),
		},
		revs: []charmrepo.CharmRevision{{
			Revision: 0,
			Sha256:   wordpressHash,
		}, {
			Revision: 0,
			Sha256:   mysqlHash,
		}, {
			Err: charmrepo.CharmNotFound("cs:~dalek/trusty/no-such"),
		}, {
			Revision: 3,
			Sha256:   riakHash,
		}},
	}}

	// Run the tests.
	for i, test := range tests {
		c.Logf("test %d: %s", i, test.about)
		revs, err := s.repo.Latest(test.urls...)
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(revs, jc.DeepEquals, test.revs)
	}
}

func (s *charmStoreRepoSuite) TestResolve(c *gc.C) {
	// Add some charms to the charm store.
	s.addCharm(c, "~who/trusty/mysql", "mysql")
	// Use different charms so that revision is actually increased.
	s.addCharm(c, "~who/precise/wordpress", "logging")
	s.addCharm(c, "~who/precise/wordpress", "wordpress")
	s.addCharm(c, "~dalek/utopic/riak", "wordpress")
	s.addCharm(c, "~dalek/utopic/riak", "riak")
	s.addCharm(c, "~dalek/utopic/riak", "wordpress")
	s.addCharm(c, "~dalek/utopic/riak", "riak")

	// Define the tests to be run.
	tests := []struct {
		id  string
		url string
		err string
	}{{
		id:  "~who/mysql",
		url: "cs:~who/trusty/mysql",
	}, {
		id:  "~who/trusty/mysql",
		url: "cs:~who/trusty/mysql",
	}, {
		id:  "~who/wordpress",
		url: "cs:~who/precise/wordpress",
	}, {
		id:  "~who/wordpress-1",
		url: "cs:~who/precise/wordpress-1",
	}, {
		id:  "~dalek/riak",
		url: "cs:~dalek/utopic/riak",
	}, {
		id:  "~dalek/utopic/riak-2",
		url: "cs:~dalek/utopic/riak-2",
	}, {
		id:  "no-such",
		err: `cannot resolve charm URL "cs:no-such": charm not found`,
	}}

	// Run the tests.
	for i, test := range tests {
		c.Logf("test %d: %s", i, test.id)
		url, err := s.repo.Resolve(charm.MustParseReference(test.id))
		if test.err != "" {
			c.Assert(err.Error(), gc.Equals, test.err)
			c.Assert(url, gc.IsNil)
			continue
		}
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(url, jc.DeepEquals, charm.MustParseURL(test.url))
	}
}

// checkCharmDownloads checks that the charm represented by the given URL has
// been downloaded the expected number of times.
func (s *charmStoreRepoSuite) checkCharmDownloads(c *gc.C, url *charm.URL, expect int) {
	client := csclient.New(csclient.Params{
		URL: s.srv.URL,
	})

	key := []string{params.StatsArchiveDownload, url.Series, url.Name, url.User, strconv.Itoa(url.Revision)}
	path := "/stats/counter/" + strings.Join(key, ":")
	var count int

	getDownloads := func() int {
		var result []params.Statistic
		err := client.Get(path, &result)
		c.Assert(err, jc.ErrorIsNil)
		return int(result[0].Count)
	}

	for retry := 0; retry < 10; retry++ {
		time.Sleep(100 * time.Millisecond)
		if count = getDownloads(); count == expect {
			if expect == 0 && retry < 2 {
				// Wait a bit to make sure.
				continue
			}
			return
		}
	}
	c.Errorf("downloads count for %s is %d, expected %d", url, count, expect)
}

// newServer instantiates a new charm store server.
func newServer(c *gc.C, session *mgo.Session) *httptest.Server {
	db := session.DB("charm-testing")
	handler, err := charmstore.NewServer(db, nil, "", serverParams, charmstore.V4)
	c.Assert(err, jc.ErrorIsNil)
	return httptest.NewServer(handler)
}

// hashOfCharm returns the SHA256 hash sum for the given charm name.
func hashOfCharm(c *gc.C, name string) string {
	path := TestCharms.CharmArchivePath(c.MkDir(), name)
	return hashOfPath(c, path)
}

// hashOfPath returns the SHA256 hash sum for the given path.
func hashOfPath(c *gc.C, path string) string {
	f, err := os.Open(path)
	c.Assert(err, jc.ErrorIsNil)
	defer f.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, f)
	c.Assert(err, jc.ErrorIsNil)
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// checkCharm checks that the given charms have the same attributes.
func checkCharm(c *gc.C, ch, expect charm.Charm) {
	c.Assert(ch.Actions(), jc.DeepEquals, expect.Actions())
	c.Assert(ch.Config(), jc.DeepEquals, expect.Config())
	c.Assert(ch.Meta(), jc.DeepEquals, expect.Meta())
}
