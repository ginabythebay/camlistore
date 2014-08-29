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

/*
Importer for pinboard.in, using the v1 api documented here:  https://pinboard.in/api.

Note that the api document seems to use 'post' and 'bookmark'
interchangeably.  We use 'post' everywhere in this code.

Posts in pinboard are mutable; they can be edited or deleted.

We handle edited posts by always reimporting everything and rewriting
any nodes.  Perhaps this would become more efficient if we would first
compare the meta tag from pinboard to the meta tag we have stored to
only write the node if there are changes.

We don't handle deleted posts.  One possible approach for this would
be to import everything under a new permanode, then once it is
successful, swap the new permanode and the posts node (note: I don't
think I really understand the data model here, so this is sort of
gibberish).

I have exchanged email with Maciej Ceglowski of pinboard, who may in
the future provide an api that lets us query what has changed.  We
might want to switch to that when available to make the import process
more light-weight.

*/

package pinboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"camlistore.org/pkg/context"
	"camlistore.org/pkg/httputil"
	"camlistore.org/pkg/importer"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/schema/nodeattr"
)

func init() {
	importer.Register("pinboard", &imp{})
}

const (
	fetchUrl = "https://api.pinboard.in/v1/posts/all?auth_token=%s&format=json&results=%d&todt=%s"

	timeFormat = "2006-01-02T15:04:05Z"
	// The start of time, in pinboard's schema
	startCursor = "0001-01-01T00:00:00Z"

	// Yes this is long.  It is what the api document suggests
	pauseInterval = 5 * time.Minute

	// Just a wild guess at what makes sense
	batchLimit = 10000

	attrAuthToken = "authToken"
)

// We expect <username>:<some id>.  Sometimes pinboard calls this an
// auth token and sometimes they call it an api token.
func extractUsername(authToken string) string {
	split := strings.SplitN(authToken, ":", 2)
	if len(split) == 2 {
		return split[0]
	} else {
		return ""
	}
}

func getAuthToken(acct *importer.Object) string {
	// will return "" if nothing is set up yet
	return acct.Attr(attrAuthToken)
}

type imp struct {
	importer.OAuth1 // for CallbackRequestAccount and CallbackURLParameters
}

func (*imp) SupportsIncremental() bool { return false }

func (*imp) NeedsAPIKey() bool { return false }

func (*imp) IsAccountReady(acct *importer.Object) (ready bool, err error) {
	ready = getAuthToken(acct) != ""
	return ready, nil
}

func (im *imp) SummarizeAccount(acct *importer.Object) string {
	ok, err := im.IsAccountReady(acct)
	if err != nil {
		return "Not configured; error = " + err.Error()
	}
	if !ok {
		return "Not configured"
	}
	return fmt.Sprintf("Pinboard account for %s", extractUsername(getAuthToken(acct)))
}

func (*imp) ServeSetup(w http.ResponseWriter, r *http.Request, ctx *importer.SetupContext) error {
	return tmpl.ExecuteTemplate(w, "serveSetup", ctx)
}

var tmpl = template.Must(template.New("root").Parse(`
{{define "serveSetup"}}
<h1>Configuring Pinboad Account</h1>
<form method="get" action="{{.CallbackURL}}">
  <input type="hidden" name="acct" value="{{.AccountNode.PermanodeRef}}">
  <table border=0 cellpadding=3>
  <tr><td align=right>API token</td><td><input name="apiToken" size=50> (You can find it <a href="https://pinboard.in/settings/password">here</a>)</td></tr>
  <tr><td align=right></td><td><input type="submit" value="Add"></td></tr>
  </table>
</form>
{{end}}
`))

func (im *imp) ServeCallback(w http.ResponseWriter, r *http.Request, ctx *importer.SetupContext) {
	t := r.FormValue("apiToken")
	if t == "" {
		http.Error(w, "Expected an API Token", 400)
		return
	}
	if err := ctx.AccountNode.SetAttrs(
		attrAuthToken, t,
	); err != nil {
		httputil.ServeError(w, r, fmt.Errorf("Error setting attribute: %v", err))
		return
	}
	http.Redirect(w, r, ctx.AccountURL(), http.StatusFound)
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

type run struct {
	*importer.RunContext
	im *imp

	// The next batch should fetch items with timestamps up to this
	nextCursor string

	// We should not fetch the next batch until this time
	nextAfter time.Time
}

func (r *run) getPostsNode() (*importer.Object, error) {
	username := extractUsername(getAuthToken(r.AccountNode()))
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

func (r *run) importPost(post *apiPost, parent *importer.Object) error {
	postNode, err := parent.ChildPathObject(post.Hash)
	if err != nil {
		return err
	}

	t, err := time.Parse(timeFormat, post.Time)
	if err != nil {
		return err
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
		return err
	}
	if err = postNode.SetAttrValues("tag", strings.Split(post.Tags, " ")); err != nil {
		return err
	}

	// TODO(gina) look over importer/attrs.go and see what from there we should be using

	return nil
}

func (r *run) importBatch(authToken string, parent *importer.Object) (keepTrying bool, err error) {
	start := time.Now()
	// TODO(gina) make the duration dynamic so we can increase it when there is a 429
	sleepDuration := r.nextAfter.Sub(time.Now())
	// block until we either get canceled or until it is time to run
	select {
	case <-r.Done():
		log.Printf("Pinboard importer: interrupted")
		return false, context.ErrCanceled
	case <-time.After(sleepDuration):
		// just proceed
	}
	// fetch a batch and process the results
	u := fmt.Sprintf(fetchUrl, authToken, batchLimit, r.nextCursor)
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
		err := r.importPost(&post, parent)
		if err != nil {
			return false, err
		}
	}

	log.Printf("Imported batch of %d posts in %s", postCount, time.Now().Sub(start))

	r.nextCursor = postBatch[postCount-1].Time
	r.nextAfter = time.Now().Add(pauseInterval)
	return true, nil
}

func (r *run) importPosts() (*importer.Object, error) {
	authToken := getAuthToken(r.AccountNode())
	parent, err := r.getPostsNode()
	if err != nil {
		return nil, err
	}

	keepTrying := true
	for keepTrying {
		keepTrying, err = r.importBatch(authToken, parent)
		if err != nil {
			return nil, err
		}
	}

	return parent, nil
}
