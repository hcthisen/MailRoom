package ui_test

import (
	"fmt"
	"net/http"
	"testing"
)

// New broadcast offers earlier broadcasts and saved emails to start from;
// choosing one prefills the form.
func Test_broadcasts_startFrom(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := createBroadcast(t, c, "Autumn")
	saved := contentCreateEmail(t, c, "Template A", "Saved subject", "Saved **body**")

	c.get("/admin/broadcasts/new").want(t, http.StatusOK).contains(t, "Start from",
		fmt.Sprintf(`href="/admin/broadcasts/new?from=broadcast:%d"`, id), fmt.Sprintf(`href="/admin/broadcasts/new?from=email:%d"`, saved))
	c.get(fmt.Sprintf("/admin/broadcasts/new?from=broadcast:%d", id)).want(t, http.StatusOK).
		contains(t, `value="Copy of Autumn"`).lacks(t, "Start from")
	c.get(fmt.Sprintf("/admin/broadcasts/new?from=email:%d", saved)).want(t, http.StatusOK).
		contains(t, `value="Saved subject"`, "Saved **body**")
	c.get("/admin/broadcasts/new?from=email:999").want(t, http.StatusOK)
}
