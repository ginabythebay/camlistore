/*
Copyright 2013 The Camlistore Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package pinboard implements a pinboard.in importer, using https://pinboard.in/api.
package pinboard

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/context"
	"camlistore.org/pkg/httputil"
	"camlistore.org/pkg/importer"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/schema/nodeattr"
)

func init() {
	importer.Register("pinboard", &imp{})
}

// imp is the pinboard importer, as a demo of how to write an importer.
//
// It must implement the importer.Importer interface in order for
// it to be registered (in the init above).
type imp struct {
	// TODO(gina) figure out what goes here.  Seems like we might not
	// need anything.  There is account state, but that isn't supposed
	// to go here.

	// The struct or underlying type implementing an importer
	// holds state that is global, and not per-account, so it
	// should not be used to cache account-specific
	// resources. Some importers (e.g. Foursquare) use this space
	// to cache mappings from site-specific global resource URLs
	// (e.g. category icons) to the fileref once it's been copied
	// into Camlistore.

}

func (*imp) SupportsIncremental() bool {
	// TODO(gina) talk to pinboard person, see if there is something
	// that makes sense here.  The posts/all call can return things
	// created since a given time, so it is tempting to think of using
	// that for incremental calls.  But what about things that have
	// been modified (e.g. given different tags?)
	return false
}

func (*imp) NeedsAPIKey() bool { return false }

const (
	// TODO(gina) nuke this
	username = "ginabythebay"

	// test with 10 results per batch for now.
	// FIXME(gina) set to something like 1000 before checking in

	// TODO(gina) collect user auth token, fill it in
	fetchUrl = "https://api.pinboard.in/v1/posts/all?auth_token=ginabythebay:e67ec74cf4a829a233ae&format=json&results=10&todt=%s"

	timeFormat = "2006-01-02T15:04:05Z"
	// The start of time, in pinboard's schema
	startCursor = "0001-01-01T00:00:00Z"

	pauseInterval = time.Minute

	// Just a wild guess at what makes sense
	batchLimit = 100
)

func (*imp) IsAccountReady(acct *importer.Object) (ready bool, err error) {
	// TODO(gina) implement this once we have the idea of accounts!
	return true, nil
}

func (*imp) SummarizeAccount(acct *importer.Object) string {
	return username
}

func (*imp) ServeSetup(w http.ResponseWriter, r *http.Request, ctx *importer.SetupContext) error {
	return nil
}

// Statically declare that our importer supports the optional
// importer.ImporterSetupHTMLer interface.
//
// We do this in case importer.ImporterSetupHTMLer changes, or if we
// typo the method name below. It turns this into a compile-time
// error. In general you should do this in Go whenever you implement
// optional interfaces.
//var _ importer.ImporterSetupHTMLer = (*imp)(nil)

// TODO(gina) implement this
func (im *imp) AccountSetupHTML(host *importer.Host) string {
	return "<h1>Hello from the pinboard importer!</h1><p>I am example HTML. This importer is a demo of how to write an importer.</p>"
}

func (im *imp) ServeCallback(w http.ResponseWriter, r *http.Request, ctx *importer.SetupContext) {
}

func (im *imp) Run(ctx *importer.RunContext) (err error) {
	log.Printf("Running pinboard importer.")
	defer func() {
		log.Printf("Pinboard importer returned: %v", err)
	}()
	r := &run{
		RunContext: ctx,
		im:         im,
		nextCursor: time.Now().Format(timeFormat),
		nextAfter:  time.Now(),
	}
	_, err = r.importPosts()
	return
}

func (im *imp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httputil.BadRequestError(w, "Unexpected path: %s", r.URL.Path)
}

func (im *imp) CallbackRequestAccount(r *http.Request) (blob.Ref, error) {
	return blob.ParseOrZero(""), nil
}

func (im *imp) CallbackURLParameters(acctRef blob.Ref) url.Values {
	return url.Values{}
}

type run struct {
	*importer.RunContext
	im *imp

	// The next batch should fetch items with timestamps up to this
	nextCursor string

	// We should not fetch the next batch until this time
	nextAfter time.Time
}

func (r *run) getPostsNode() (*importer.Object, error) {
	root := r.RootNode()
	rootTitle := fmt.Sprintf("%s's Pinboard Account", username)
	log.Printf("root title = %q; want %q", root.Attr(nodeattr.Title), rootTitle)
	if err := root.SetAttr(nodeattr.Title, rootTitle); err != nil {
		return nil, err
	}
	obj, err := root.ChildPathObject("posts")
	if err != nil {
		return nil, err
	}
	title := fmt.Sprintf("%s's Posts", username)
	return obj, obj.SetAttr(nodeattr.Title, title)
}

type apiPost struct {
	Href        string
	Description string
	// what is this?
	Extended string
	Meta     string
	Hash     string
	Time     string
	Shared   string
	ToRead   string
	// how are multi tags handled?
	Tags string
}

func (r *run) importBatch(parent *importer.Object) (keepTrying bool, err error) {
	// TODO(gina) make the duration dynamic so we can increase it when there is a 429
	sleepDuration := r.nextAfter.Sub(time.Now())
	log.Printf("entered batch.  Now=%v, nextAfter=%v, sleepDuration=%v", time.Now(), r.nextAfter, sleepDuration)
	// block until we either get canceled or until it is time to run
	select {
	case <-r.Done():
		log.Printf("Pinboard importer: interrupted")
		return false, context.ErrCanceled
	case <-time.After(sleepDuration):
		// just proceed
	}
	// fetch a batch and process the results
	u := fmt.Sprintf(fetchUrl, r.nextCursor)
	resp, err := http.Get(u)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// TODO(gina) check for 429 Too Many Requests and back off (double the timeout each time)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Unexpected status code %v fetching %v", resp.StatusCode, u)
	}
	body, err := ioutil.ReadAll(resp.Body)
	log.Print(string(body))
	if err != nil {
		return false, err
	}

	var postBatch []apiPost
	if err = json.Unmarshal(body, &postBatch); err != nil {
		return false, err
	}

	if err != nil {
		return false, err
	}

	postCount := len(postBatch)
	if postCount == 0 {
		// we are done!
		return false, nil
	}

	log.Printf("Importing %d posts...", postCount)
	for _, post := range postBatch {
		if r.Context.IsCanceled() {
			log.Printf("Pinboard importer: interrupted")
			return false, context.ErrCanceled
		}
		postNode, err := parent.ChildPathObject(post.Hash)
		if err != nil {
			return false, err
		}

		t, err := time.Parse(timeFormat, post.Time)
		if err != nil {
			return false, err
		}

		attrs := []string{
			"pinboard.in:hash", post.Hash,
			nodeattr.Type, "pinboard.in:post",
			nodeattr.DateCreated, schema.RFC3339FromTime(t),
			nodeattr.Title, post.Description,
			nodeattr.URL, post.Href,
			"pinboard.in:extended", post.Extended,
			"pinboard.in:meta", post.Meta,
			"pinboard.in:shared", post.Shared,
			"pinboard.in:toread", post.ToRead,
		}
		if err = postNode.SetAttrs(attrs...); err != nil {
			return false, err
		}
		if err = postNode.SetAttrValues("tag", strings.Split(post.Tags, " ")); err != nil {
			return false, err
		}

		// TODO(gina) look over importer/attrs.go and see what from there we should be using
	}

	r.nextCursor = postBatch[postCount-1].Time
	r.nextAfter = time.Now().Add(pauseInterval)
	return true, nil
}

func (r *run) importPosts() (*importer.Object, error) {
	parent, err := r.getPostsNode()
	if err != nil {
		return nil, err
	}

	keepTrying := true
	for keepTrying {
		keepTrying, err = r.importBatch(parent)
		if err != nil {
			return nil, err
		}
	}

	return parent, nil
}
