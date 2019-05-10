/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/options"
	bpb "github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/dgo"
	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/ee/backup"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/dgraph/z"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	dirs = []string{
		"./data/restore",
	}

	alphaContainers = []string{
		"alpha1",
		"alpha2",
		"alpha3",
	}

	original *api.Assigned
	incr1    *api.Assigned
	incr2    *api.Assigned
)

func TestBackupFilesystem(t *testing.T) {
	conn, err := grpc.Dial(z.SockAddr, grpc.WithInsecure())
	x.Check(err)
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))

	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `movie: string .`}))
	original, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			<_:x1> <movie> "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)" .
			<_:x2> <movie> "Spotlight" .
			<_:x3> <movie> "Moonlight" .
			<_:x4> <movie> "THE SHAPE OF WATERLOO" .
			<_:x5> <movie> "BLACK PUNTER" .
		`),
	})
	require.NoError(t, err)
	t.Logf("--- Original uid mapping: %+v\n", original.Uids)

	// move tablet to group 1 to avoid messes later.
	_, err = http.Get("http://" + z.SockAddrZeroHttp + "/moveTablet?tablet=movie&group=1")
	require.NoError(t, err)
	// if the move happened, we need to pause a bit to give zero a chance to quorum
	t.Log("Pausing to let zero move tablet...")
	for retry := 5; retry > 0; retry-- {
		time.Sleep(3 * time.Second)
		state, err := getState()
		require.NoError(t, err)
		if _, ok := state.Groups["1"].Tablets["movie"]; ok {
			break
		}
	}

	dirSetup()

	resp, err := http.PostForm("http://localhost:8180/admin/backup", url.Values{
		"destination": []string{"/data/backups"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, getError(resp.Body))
	time.Sleep(5 * time.Second)

	// There must be a backup file for each three groups.
	files := x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup")
	})
	require.True(t, len(files) == 3)

	dirs := x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups/dgraph.")
	})
	require.True(t, len(dirs) == 1)

	// Restore this backup dir (3 files total).
	t.Logf("--- Restoring from: %q", dirs[0])
	_, err = backup.RunRestore("./data/restore", dirs[0])
	require.NoError(t, err)

	// just check p1 which should have the 'movie' predicate (moved during setup)
	restored, err := getPValues("./data/restore/p1", "movie", math.MaxUint64)
	require.NoError(t, err)
	t.Logf("--- Restored values: %+v\n", restored)

	tests := []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)"},
		{blank: "x2", expected: "Spotlight"},
		{blank: "x3", expected: "Moonlight"},
		{blank: "x4", expected: "THE SHAPE OF WATERLOO"},
		{blank: "x5", expected: "BLACK PUNTER"},
	}
	for _, tc := range tests {
		require.EqualValues(t, tc.expected, restored[original.Uids[tc.blank]])
	}

	// Do a mutation.
	incr1, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
			<%s> <movie> "Birdman or (The Unexpected Virtue of Ignorance)" .
			<%s> <movie> "The Shape of Waterloo" .
		`, original.Uids["x1"], original.Uids["x4"])),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)
	time.Sleep(5 * time.Second)

	resp, err = http.PostForm("http://localhost:8180/admin/backup", url.Values{
		"destination": []string{"/data/backups"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, getError(resp.Body))
	time.Sleep(5 * time.Second)

	// there must be a backup file for each 3 groups
	files = x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup")
	})
	require.True(t, len(files) == 6)

	dirs = x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups/dgraph.")
	})
	require.True(t, len(dirs) == 2)

	// Restore this backup dir (3 files total).
	t.Logf("--- Restoring from: %q", dirs[1])
	_, err = backup.RunRestore("./data/restore", dirs[1])
	require.NoError(t, err)

	// Just check p1 which should have the 'movie' predicate (moved during setup).
	restored, err = getPValues("./data/restore/p1", "movie", incr1.Context.CommitTs)
	require.NoError(t, err)
	t.Logf("--- Restored values: %+v\n", restored)

	tests = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "Birdman or (The Unexpected Virtue of Ignorance)"},
		{blank: "x4", expected: "The Shape of Waterloo"},
	}
	for _, tc := range tests {
		require.EqualValues(t, tc.expected, restored[original.Uids[tc.blank]])
	}

	incr2, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
				<%s> <movie> "The Shape of Water" .
				<%s> <movie> "The Black Panther" .
			`, original.Uids["x4"], original.Uids["x5"])),
	})
	require.NoError(t, err)
	time.Sleep(5 * time.Second)

	resp, err = http.PostForm("http://localhost:8180/admin/backup", url.Values{
		"destination": []string{"/data/backups"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, getError(resp.Body))
	time.Sleep(5 * time.Second)

	// there must be a backup file for each 3 groups
	files = x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup")
	})
	require.True(t, len(files) == 9)

	dirs = x.WalkPathFunc("./data/backups", func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups/dgraph.")
	})
	require.True(t, len(dirs) == 3)

	// Restore this backup dir (3 files total).
	t.Logf("--- Restoring from: %q", dirs[2])
	_, err = backup.RunRestore("./data/restore", dirs[2])
	require.NoError(t, err)

	// just check p1 which should have the 'movie' predicate (moved during setup)
	restored, err = getPValues("./data/restore/p1", "movie", incr2.Context.CommitTs)
	require.NoError(t, err)
	t.Logf("--- Restored values: %+v\n", restored)

	tests = []struct {
		blank, expected string
	}{
		{blank: "x4", expected: "The Shape of Water"},
		{blank: "x5", expected: "The Black Panther"},
	}
	for _, tc := range tests {
		require.EqualValues(t, tc.expected, restored[original.Uids[tc.blank]])
	}

	dirCleanup()
}

func getError(rc io.ReadCloser) error {
	defer rc.Close()
	b, err := ioutil.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("Read failed: %v", err)
	}
	if bytes.Contains(b, []byte("Error")) {
		return fmt.Errorf("%s", string(b))
	}
	return nil
}

type response struct {
	Groups map[string]struct {
		Members map[string]interface{} `json:"members"`
		Tablets map[string]struct {
			GroupID   int    `json:"groupId"`
			Predicate string `json:"predicate"`
		} `json:"tablets"`
	} `json:"groups"`
	Removed []struct {
		Addr    string `json:"addr"`
		GroupID int    `json:"groupId"`
		ID      string `json:"id"`
	} `json:"removed"`
}

func getState() (*response, error) {
	resp, err := http.Get("http://" + z.SockAddrZeroHttp + "/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if bytes.Contains(b, []byte("Error")) {
		return nil, fmt.Errorf("Failed to get state: %s", string(b))
	}

	var st response
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func getPValues(pdir, attr string, readTs uint64) (map[string]string, error) {
	opt := badger.DefaultOptions
	opt.Dir = pdir
	opt.ValueDir = pdir
	opt.TableLoadingMode = options.MemoryMap
	opt.ReadOnly = true
	db, err := badger.OpenManaged(opt)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	values := make(map[string]string)

	stream := db.NewStreamAt(math.MaxUint64)
	stream.ChooseKey = func(item *badger.Item) bool {
		pk := x.Parse(item.Key())
		switch {
		case pk.Attr != attr:
			return false
		case pk.IsSchema():
			return false
		}
		return pk.IsData()
	}
	stream.KeyToList = func(key []byte, it *badger.Iterator) (*bpb.KVList, error) {
		pk := x.Parse(key)
		pl, err := posting.ReadPostingList(key, it)
		if err != nil {
			return nil, err
		}
		var list bpb.KVList
		err = pl.Iterate(readTs, 0, func(p *pb.Posting) error {
			vID := types.TypeID(p.ValType)
			src := types.ValueForType(vID)
			src.Value = p.Value
			str, err := types.Convert(src, types.StringID)
			if err != nil {
				fmt.Println(err)
				return nil
			}
			value := strings.TrimRight(str.Value.(string), "\x00")
			list.Kv = append(list.Kv, &bpb.KV{
				Key:   []byte(fmt.Sprintf("%#x", pk.Uid)),
				Value: []byte(value),
			})
			return nil
		})
		return &list, err
	}
	stream.Send = func(list *bpb.KVList) error {
		for _, kv := range list.Kv {
			values[string(kv.Key)] = string(kv.Value)
		}
		return nil
	}
	if err := stream.Orchestrate(context.Background()); err != nil {
		return nil, err
	}
	return values, err
}

func dirSetup() {
	for i := range dirs {
		x.Check(os.MkdirAll(dirs[i], 777))
	}
}

func dirCleanup() {
	for i := range dirs {
		x.Check(os.RemoveAll(dirs[i]))
	}
}
