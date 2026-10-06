package forms

import (
	"errors"
	"net/http"
)

// handleConfirmPage shows the confirmation button. It changes nothing, so
// mail scanners and link prefetchers that open the link don't confirm.
func (s *Service) handleConfirmPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.allowPage(w, r, false) {
		return
	}
	token := r.PathValue("token")
	kind, err := s.CheckConfirmation(r.Context(), token)
	if err != nil {
		s.writeLinkError(w, r, err)
		return
	}
	d := pageData{
		Heading:       "Confirm your subscription",
		Message:       "Click the button to confirm that you want to receive our emails.",
		ConfirmAction: s.confirmURL(token),
		ConfirmLabel:  "Confirm subscription",
	}
	if kind == KindResubscribe {
		d.Heading = "Confirm that you want our emails again"
		d.Message = "You unsubscribed earlier. Click the button to receive our emails again."
		d.ConfirmLabel = "Yes, send me emails again"
	}
	s.writePage(w, r, http.StatusOK, d)
}

// handleConfirm confirms a double opt-in or resubscribe.
func (s *Service) handleConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.allowPage(w, r, false) {
		return
	}
	ctx := publicCtx(r)
	r = r.WithContext(ctx)
	ip, _ := s.clientKey(r)
	res, err := s.Confirm(ctx, r.PathValue("token"), ip, r.UserAgent())
	if err != nil {
		s.writeLinkError(w, r, err)
		return
	}
	if res.Kind == KindResubscribe {
		s.writeMessage(w, r, http.StatusOK, "Welcome back!", "You're subscribed again and will receive our emails. You can unsubscribe at any time with the link in every email.")
		return
	}
	s.writeMessage(w, r, http.StatusOK, "You're subscribed!", "Thank you for confirming. You can unsubscribe at any time with the link in every email.")
}

// writeLinkError explains a confirmation link that can't be used.
func (s *Service) writeLinkError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrLinkInvalid):
		s.writeMessage(w, r, http.StatusNotFound, "Link not valid",
			"This confirmation link is not valid. Please check that you opened the whole link from the email.")
	case errors.Is(err, ErrLinkExpired):
		s.writeMessage(w, r, http.StatusGone, "Link expired",
			"This confirmation link has expired. Please sign up again to get a new one.")
	case errors.Is(err, ErrLinkUsed):
		s.writeMessage(w, r, http.StatusGone, "Already confirmed",
			"This link has already been used, so there is nothing more to do.")
	case errors.Is(err, ErrLinkSuperseded):
		s.writeMessage(w, r, http.StatusGone, "Link replaced",
			"We sent you a newer confirmation email. Please use the link in the most recent one.")
	case errors.Is(err, ErrCannotConfirm):
		s.writeMessage(w, r, http.StatusGone, "Can't confirm",
			"This address can't be subscribed with this link any more. If you think this is a mistake, please contact us.")
	default:
		s.internalError(w, r, false, err)
	}
}
