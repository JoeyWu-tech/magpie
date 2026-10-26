package middleware

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/yetone/magpie/internal/gwmw"
)

// A hook can end just as its time runs out: the timer's func has begun and
// not yet given its interrupt. A runtime has one timer for all its calls,
// and Stop doesn't wait for a func that has begun, so the func went on to
// stop the next call on the runtime, though that call had time left: the
// next event of a reply, or another request's hook (#3742). Here onRequest
// ends as its time runs out, the timer's func held then, and the func is
// let go once onRequest's disarm waits for it or the onProvider after is
// in its hook. An onRequest that finished keeps what it returned, one that
// failed reads took too long, and the two onProvider calls after answer.
func TestTimeRunsOutAsTheHookEnds(t *testing.T) {
	for _, c := range []struct {
		name string
		ends string // how onRequest ends, its time run out
		want string // the body Request gives
		last string // the middleware's last failure
	}{
		{"finished", "body.r = 1; return body;", `{"r":1}`, ""},
		{"failed", `throw new Error("the module threw");`, `{}`, "onRequest: took too long"},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				begun, release, gave := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var hold atomic.Bool
				afterFunc = func(d time.Duration, f func()) *time.Timer {
					return time.AfterFunc(d, func() {
						if !hold.CompareAndSwap(true, false) {
							f()
							return
						}
						close(begun)
						<-release
						f()
						close(gave)
					})
				}
				t.Cleanup(func() { afterFunc = time.AfterFunc })
				e := file(t, `export function onRequest(body) { outOfTime(); `+c.ends+` }
export function onProvider(body) { letGo(); body.p = 1; return body; }`)
				use(t, e)
				r := Begin(info())
				defer r.End()
				in, _, err := r.acquire(0)
				if err != nil {
					t.Fatal(err)
				}
				// outOfTime returns once the hook's time has run out, letGo
				// once the test lets it go
				goOn := make(chan struct{})
				for name, c := range map[string]chan struct{}{"outOfTime": begun, "letGo": goOn} {
					if err := in.rt.SetGlobal(name, moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
						<-c
						return moejs.Undefined(), nil
					})); err != nil {
						t.Fatal(err)
					}
				}
				hold.Store(true)
				var req, prov, again []byte
				done := make(chan struct{})
				go func() {
					defer close(done)
					req, _ = r.Request([]byte(`{}`))
					prov = r.Tried([]byte(`{}`), gwmw.To{ID: "p1", Model: "m1"})
					again = r.Tried([]byte(`{}`), gwmw.To{ID: "p2", Model: "m1"})
				}()
				<-begun
				synctest.Wait() // onRequest's disarm waits for the func, or onProvider is in letGo
				close(release)
				<-gave
				synctest.Wait() // onProvider is in letGo
				close(goOn)
				<-done
				st := States()[e.Spec]
				if string(req) != c.want {
					t.Errorf("onRequest, its time run out as it ended, gave %s, want %s", req, c.want)
				}
				if string(prov) != `{"p":1}` || string(again) != `{"p":1}` {
					t.Errorf(`the onProvider calls after gave %s and %s, want {"p":1}; last failure %q`, prov, again, st.Last)
				}
				if st.Last != c.last {
					t.Errorf("last failure %q, want %q", st.Last, c.last)
				}
			})
		})
	}
}
