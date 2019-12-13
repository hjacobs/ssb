// SPDX-License-Identifier: MIT

package sbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.cryptoscope.co/margaret"

	"go.cryptoscope.co/ssb"
	"go.cryptoscope.co/ssb/blobstore"
	"go.cryptoscope.co/ssb/internal/leakcheck"
	"go.cryptoscope.co/ssb/internal/testutils"
	"go.cryptoscope.co/ssb/network"
)

const blobSize = 1024 * 512

func TestBlobsPair(t *testing.T) {
	defer leakcheck.Check(t)
	r := require.New(t)
	ctx, cancel := context.WithCancel(context.TODO())

	os.RemoveAll("testrun")

	appKey := make([]byte, 32)
	rand.Read(appKey)
	hmacKey := make([]byte, 32)
	rand.Read(hmacKey)

	info := testutils.NewRelativeTimeLogger(nil)

	aliLog := log.With(info, "peer", "ali")
	ali, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(aliLog),
		// WithConnWrapper(func(conn net.Conn) (net.Conn, error) {
		// 	return debug.WrapConn(log.With(aliLog, "who", "a"), conn), nil
		// }),
		WithRepoPath(filepath.Join("testrun", t.Name(), "ali")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	ali.ServeAll(ctx)

	bobLog := log.With(info, "peer", "bob")
	bob, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(bobLog),
		// WithConnWrapper(func(conn net.Conn) (net.Conn, error) {
		// 	return debug.WrapConn(bobLog, conn), nil
		// }),
		WithRepoPath(filepath.Join("testrun", t.Name(), "bob")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	bob.ServeAll(ctx)

	seq, err := ali.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   bob.KeyPair.Id,
	})
	r.NoError(err)
	r.Equal(margaret.BaseSeq(0), seq)

	seq, err = bob.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   ali.KeyPair.Id,
	})
	r.NoError(err)

	g, err := bob.GraphBuilder.Build()
	r.NoError(err)
	time.Sleep(250 * time.Millisecond)
	r.True(g.Follows(bob.KeyPair.Id, ali.KeyPair.Id))

	sess := &session{
		ctx:   ctx,
		alice: ali,
		bob:   bob,
		redial: func(t *testing.T) {
			t.Log("noop")
		},
	}

	tests := []struct {
		name string
		tf   func(t *testing.T)
	}{
		{"simple", sess.simple},
		{"wantFirst", sess.wantFirst},
		{"eachOne", sess.eachOne},
		{"eachOneConnet", sess.eachOneConnet},
		{"eachOneBothWant", sess.eachOnBothWant},
	}

	// all on a single connection
	err = bob.Connect(ctx, ali.GetListenAddr(ssb.NetworkScopePublic))
	fmt.Println("dialed!")
	r.NoError(err)
	for _, tc := range tests {
		t.Run("noop/"+tc.name, tc.tf)
	}

	info.Log("block1", "done")

	aliCT := ali.GetConnTracker(ssb.NetworkScopePublic)
	bobCT := bob.GetConnTracker(ssb.NetworkScopePublic)
	aliCT.CloseAll()
	bobCT.CloseAll()
	i := 0
	for aliCT.Count() != 0 || bobCT.Count() != 0 {
		time.Sleep(750 * time.Millisecond)
		info.Log("XXXX", "waited after close", "i", i, "a", aliCT.Count(), "b", bobCT.Count())
		i++
		if i > 10 {
			t.Fatal("retried waiting for close")
		}
	}

	// disconnect and reconnect
	sess.redial = func(t *testing.T) {
		aliCT.CloseAll()
		bobCT.CloseAll()
		time.Sleep(1 * time.Second)
		assert.EqualValues(t, 0, aliCT.Count(), "a: not all closed")
		assert.EqualValues(t, 0, bobCT.Count(), "b: not all closed")
		err := bob.Connect(ctx, ali.GetListenAddr(ssb.NetworkScopePublic))
		r.NoError(err)
		time.Sleep(2 * time.Second)
		assert.EqualValues(t, 1, aliCT.Count(), "a: want 1 conn")
		assert.EqualValues(t, 1, bobCT.Count(), "b: want 1 conn")
	}
	for _, tc := range tests {
		t.Run("dcFirst/"+tc.name, tc.tf)
	}

	info.Log("block2", "done")

	aliCT.CloseAll()
	bobCT.CloseAll()
	time.Sleep(2 * time.Second)
	assert.EqualValues(t, 0, aliCT.Count(), "a: not all closed")
	assert.EqualValues(t, 0, bobCT.Count(), "b: not all closed")

	/* TODO: this fails _sometimes_
	// just re-dial
	sess.redial = func(t *testing.T) {
		info.Log("redial", "b>a")
		err = bob.Network.Connect(ctx, ali.Network.GetListenAddr())
		r.NoError(err)
		i := 0
		for aliCT.Count() < 1 || bobCT.Count() < 1 {
			time.Sleep(750 * time.Millisecond)
			info.Log("debugwait", "waited after connect", "i", i, "a", aliCT.Count(), "b", bobCT.Count())
			i++
			if i > 10 {
				info.Log("fail", "waited for conns failed")
				t.Fatal("retried dialing")
			}
		}

	}
	for _, tc := range tests {
		t.Run("redial/"+tc.name, tc.tf)
	}
	info.Log("block3", "done")
	*/

	ali.Shutdown()
	bob.Shutdown()

	cancel()

	r.NoError(ali.Close())
	r.NoError(bob.Close())

}

type session struct {
	ctx context.Context

	redial func(t *testing.T)

	alice, bob *Sbot
}

func (s *session) simple(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	s.redial(t)

	// blob action
	randBuf := make([]byte, blobSize)
	rand.Read(randBuf)

	ref, err := s.bob.BlobStore.Put(bytes.NewReader(randBuf))
	r.NoError(err)
	t.Log("added", ref.Ref())

	err = s.alice.WantManager.Want(ref)
	r.NoError(err)

	time.Sleep(2 * time.Second)

	_, err = s.alice.BlobStore.Get(ref)
	a.NoError(err)
}

func (s *session) wantFirst(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	// blob action
	randBuf := make([]byte, blobSize)
	rand.Read(randBuf)

	ref, err := s.bob.BlobStore.Put(bytes.NewReader(randBuf))
	r.NoError(err)
	t.Log("added", ref.Ref())

	err = s.alice.WantManager.Want(ref)
	r.NoError(err)

	s.redial(t)

	time.Sleep(2 * time.Second)

	_, err = s.alice.BlobStore.Get(ref)
	a.NoError(err)

}

func (s *session) eachOne(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	// blob action
	randOne := make([]byte, blobSize)
	rand.Read(randOne)
	refOne, err := s.bob.BlobStore.Put(bytes.NewReader(randOne))
	r.NoError(err)
	t.Log("added1", refOne.Ref())

	randTwo := make([]byte, blobSize)
	rand.Read(randTwo)
	refTwo, err := s.alice.BlobStore.Put(bytes.NewReader(randTwo))
	r.NoError(err)
	t.Log("added2", refTwo.Ref())

	s.redial(t)

	err = s.alice.WantManager.Want(refOne)
	r.NoError(err)

	err = s.bob.WantManager.Want(refTwo)
	r.NoError(err)

	time.Sleep(2 * time.Second)

	_, err = s.alice.BlobStore.Get(refOne)
	a.NoError(err)
	_, err = s.bob.BlobStore.Get(refTwo)
	a.NoError(err)
}

func (s *session) eachOneConnet(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	// blob action
	randOne := make([]byte, blobSize)
	rand.Read(randOne)
	refOne, err := s.bob.BlobStore.Put(bytes.NewReader(randOne))
	r.NoError(err)
	t.Log("added1", refOne.Ref())

	randTwo := make([]byte, blobSize)
	rand.Read(randTwo)
	refTwo, err := s.alice.BlobStore.Put(bytes.NewReader(randTwo))
	r.NoError(err)
	t.Log("added2", refTwo.Ref())

	err = s.alice.WantManager.Want(refOne)
	r.NoError(err)

	s.redial(t)

	err = s.bob.WantManager.Want(refTwo)
	r.NoError(err)

	time.Sleep(2 * time.Second)

	_, err = s.alice.BlobStore.Get(refOne)
	a.NoError(err)
	_, err = s.bob.BlobStore.Get(refTwo)
	a.NoError(err)
}

func (s *session) eachOnBothWant(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	// blob action
	randOne := make([]byte, blobSize)
	rand.Read(randOne)
	refOne, err := s.bob.BlobStore.Put(bytes.NewReader(randOne))
	r.NoError(err)
	t.Log("added1", refOne.Ref())

	randTwo := make([]byte, blobSize)
	rand.Read(randTwo)
	refTwo, err := s.alice.BlobStore.Put(bytes.NewReader(randTwo))
	r.NoError(err)
	t.Log("added2", refTwo.Ref())

	err = s.alice.WantManager.Want(refOne)
	r.NoError(err)

	err = s.bob.WantManager.Want(refTwo)
	r.NoError(err)

	s.redial(t)

	time.Sleep(2 * time.Second)

	_, err = s.alice.BlobStore.Get(refOne)
	a.NoError(err)
	_, err = s.bob.BlobStore.Get(refTwo)
	a.NoError(err)
}

// check that we can get blobs from C to A through B
func TestBlobsWithHops(t *testing.T) {
	// defer leakcheck.Check(t)
	r := require.New(t)
	a := assert.New(t)
	ctx, cancel := context.WithCancel(context.TODO())

	os.RemoveAll("testrun")

	appKey := make([]byte, 32)
	rand.Read(appKey)
	hmacKey := make([]byte, 32)
	rand.Read(hmacKey)

	mainLog := testutils.NewRelativeTimeLogger(nil)

	// make three bots (ali, bob and cle)
	ali, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(log.With(mainLog, "peer", "ali")),
		WithRepoPath(filepath.Join("testrun", t.Name(), "ali")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	ali.ServeAll(ctx)

	bob, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(log.With(mainLog, "peer", "bob")),
		WithRepoPath(filepath.Join("testrun", t.Name(), "bob")),
		// enabling this makes the tests hang but it can be insightfull to see all muxrpc packages
		// WithPostSecureConnWrapper(func(conn net.Conn) (net.Conn, error) {
		// 	addr := netwrap.GetAddr(conn.RemoteAddr(), "shs-bs")
		// 	return debug.WrapConn(log.With(mainLog, "remote", addr.String()[1:5]), conn), nil
		// }),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	bob.ServeAll(ctx)

	cle, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(log.With(mainLog, "peer", "cle")),
		WithRepoPath(filepath.Join("testrun", t.Name(), "cle")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	cle.ServeAll(ctx)

	// ali <> bob
	_, err = ali.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   bob.KeyPair.Id,
	})
	r.NoError(err)
	_, err = bob.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   ali.KeyPair.Id,
	})
	r.NoError(err)
	// bob <> cle
	_, err = bob.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   cle.KeyPair.Id,
	})
	r.NoError(err)
	_, err = cle.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   bob.KeyPair.Id,
	})
	r.NoError(err)

	time.Sleep(1 * time.Second)

	err = bob.Connect(ctx, ali.GetListenAddr(ssb.NetworkScopePublic))
	r.NoError(err)
	err = bob.Connect(ctx, cle.GetListenAddr(ssb.NetworkScopePublic))
	r.NoError(err)

	time.Sleep(1 * time.Second)

	// blob action
	n := blobSize
	randBuf := make([]byte, n)
	rand.Read(randBuf)

	ref, err := cle.BlobStore.Put(bytes.NewReader(randBuf))
	r.NoError(err)

	err = ali.WantManager.Want(ref)
	r.NoError(err)

	for i := 0; ali.WantManager.Wants(ref); i++ {
		time.Sleep(1 * time.Second)
		if i > 15 {
			t.Error("want timeout")
			break
		}
	}

	_, err = ali.BlobStore.Get(ref)
	a.NoError(err)

	sz, err := ali.BlobStore.Size(ref)
	a.NoError(err)
	a.EqualValues(n, sz)

	// too-big check

	f, err := os.Open("/dev/zero")
	r.NoError(err)
	defer f.Close()

	junkSz := int64(7 * 1024 * 1024)
	junkBlob, err := ali.BlobStore.Put(io.LimitReader(f, junkSz))
	r.NoError(err)
	sz, err = ali.BlobStore.Size(junkBlob)
	r.NoError(err)
	r.Equal(junkSz, sz)

	err = cle.WantManager.Want(junkBlob)
	r.NoError(err)

	time.Sleep(1 * time.Second)

	for i := 0; bob.WantManager.Wants(junkBlob); i++ {
		time.Sleep(1 * time.Second)
		t.Log("polling want for junkBlob...", i)
		if i > 15 {
			t.Error("timeout")
			break
		}
	}

	a.False(bob.WantManager.Wants(junkBlob))

	_, err = bob.BlobStore.Size(junkBlob)
	a.True(err == blobstore.ErrNoSuchBlob, "err T: %T %s", err, err)

	err = bob.WantManager.Want(junkBlob)
	a.Equal(err, blobstore.ErrBlobBlocked)

	a.True(cle.WantManager.Wants(junkBlob))
	_, err = cle.BlobStore.Size(junkBlob)
	a.True(err == blobstore.ErrNoSuchBlob, "err T: %T %s", err, err)

	ali.Shutdown()
	bob.Shutdown()
	cle.Shutdown()

	// TODO:
	// a.False(ali.WantManager.Wants(ref), "a still wants")
	// a.False(bob.WantManager.Wants(ref), "b still wants")
	// a.False(cle.WantManager.Wants(ref), "c still wants")

	cancel()
	r.NoError(ali.Close())
	r.NoError(bob.Close())
	r.NoError(cle.Close())
}

// TODO: make extra test to make sure this doesn't turn into an echo chamber

func TestBlobsTooBig(t *testing.T) {
	defer leakcheck.Check(t)
	r := require.New(t)
	ctx, cancel := context.WithCancel(context.TODO())

	// <testSetup>
	os.RemoveAll("testrun")

	appKey := make([]byte, 32)
	rand.Read(appKey)
	hmacKey := make([]byte, 32)
	rand.Read(hmacKey)

	info := testutils.NewRelativeTimeLogger(nil)

	aliLog := log.With(info, "peer", "ali")
	ali, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(aliLog),
		// WithConnWrapper(func(conn net.Conn) (net.Conn, error) {
		// 	return debug.WrapConn(log.With(aliLog, "who", "a"), conn), nil
		// }),
		WithRepoPath(filepath.Join("testrun", t.Name(), "ali")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	ali.ServeAll(ctx)

	bobLog := log.With(info, "peer", "bob")
	bob, err := New(
		WithAppKey(appKey),
		WithHMACSigning(hmacKey),
		WithContext(ctx),
		WithInfo(bobLog),
		// WithConnWrapper(func(conn net.Conn) (net.Conn, error) {
		// 	return debug.WrapConn(bobLog, conn), nil
		// }),
		WithRepoPath(filepath.Join("testrun", t.Name(), "bob")),
		LateOption(WithNetwork(network.Options{
			Scope:      ssb.NetworkScopePublic,
			ListenAddr: &net.TCPAddr{Port: 0},
		})),
		// LateOption(MountPlugin(&bytype.Plugin{}, plugins2.AuthMaster)),
	)
	r.NoError(err)
	bob.ServeAll(ctx)

	seq, err := ali.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   bob.KeyPair.Id,
	})
	r.NoError(err)
	r.Equal(margaret.BaseSeq(0), seq)

	seq, err = bob.PublishLog.Append(ssb.Contact{
		Type:      "contact",
		Following: true,
		Contact:   ali.KeyPair.Id,
	})
	r.NoError(err)

	err = bob.Connect(ctx, ali.GetListenAddr(ssb.NetworkScopePublic))
	r.NoError(err)
	// </testSetup>

	// A adds a very big blob
	zerof, err := os.Open("/dev/zero")
	r.NoError(err)
	defer zerof.Close()

	const smallEnough = blobstore.DefaultMaxSize - 10
	smallRef, err := ali.BlobStore.Put(io.LimitReader(zerof, smallEnough))
	r.NoError(err)
	t.Log("added small", smallRef.Ref())

	const veryLarge = blobstore.DefaultMaxSize + 10
	ref, err := ali.BlobStore.Put(io.LimitReader(zerof, veryLarge))
	r.NoError(err)
	t.Log("added too big", ref.Ref())

	sz, err := ali.BlobStore.Size(ref)
	r.NoError(err)
	r.EqualValues(veryLarge, sz)
	time.Sleep(1 * time.Second)

	err = bob.WantManager.Want(ref)
	r.NoError(err)
	err = bob.WantManager.Want(smallRef)
	r.NoError(err)

	time.Sleep(3 * time.Second)

	_, err = bob.BlobStore.Get(smallRef)
	r.NoError(err)

	_, err = bob.BlobStore.Get(ref)
	r.Error(err)
	r.Equal(err, blobstore.ErrNoSuchBlob)

	cancel()
	ali.Shutdown()
	bob.Shutdown()

	// cleanup
	r.NoError(ali.Close())
	r.NoError(bob.Close())
}
