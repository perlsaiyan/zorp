package graph

import "testing"

func TestFindJoinURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A Google-synced Zoom invite: the direct link is in an href.
		{`<a href="https://www.google.com/url?q=https%3A%2F%2Fcalendar.google.com&amp;sa=D">x</a>
		  <a href="https://us02web.zoom.us/j/4671016923?pwd=abc&amp;from=addon">Direct Join Zoom Meet</a>
		  <a href="https://zoom.us/join">Join</a>`, "https://us02web.zoom.us/j/4671016923?pwd=abc&from=addon"},
		{`<a href="https://www.google.com/url?q=https%3A%2F%2Fmeet.google.com%2Fabc-defg-hij&amp;sa=D">Join</a>`, "https://meet.google.com/abc-defg-hij"},
		{"Join: https://acme.webex.com/acme/j.php?MTID=m123.", "https://acme.webex.com/acme/j.php?MTID=m123"},
		{`https://eur01.safelinks.protection.outlook.com/?url=https%3A%2F%2Fzoom.us%2Fj%2F123&data=x`, "https://zoom.us/j/123"},
		{"Zoom", ""},
		{"https://zoom.us/ and https://meet.google.com/", ""},
	} {
		if got := FindJoinURL(tc.in); got != tc.want {
			t.Errorf("FindJoinURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	ev := Event{Location: Location{DisplayName: "Zoom"}, Body: &ItemBody{Content: `<a href="https://zoom.us/my/room">x</a>`}}
	if got := ev.JoinURL(); got != "https://zoom.us/my/room" {
		t.Errorf("JoinURL from the body = %q", got)
	}
}
