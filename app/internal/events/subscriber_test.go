package events_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"

	"mailroom/internal/events"
)

// fakeSNS serves subscription pages and records Subscribe calls.
type fakeSNS struct {
	mu         sync.Mutex
	pages      [][]snstypes.Subscription
	listErrs   []error // returned by the first ListSubscriptionsByTopic calls
	subErrs    []error // returned by the first Subscribe calls
	lists      int
	subscribes []*sns.SubscribeInput
	topics     []string
}

func (f *fakeSNS) ListSubscriptionsByTopic(_ context.Context, in *sns.ListSubscriptionsByTopicInput, _ ...func(*sns.Options)) (*sns.ListSubscriptionsByTopicOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.topics = append(f.topics, aws.ToString(in.TopicArn))
	if len(f.listErrs) > 0 {
		err := f.listErrs[0]
		f.listErrs = f.listErrs[1:]
		return nil, err
	}
	page := 0
	if in.NextToken != nil {
		for i := range f.pages {
			if *in.NextToken == pageToken(i) {
				page = i
			}
		}
	}
	out := &sns.ListSubscriptionsByTopicOutput{}
	if page < len(f.pages) {
		out.Subscriptions = f.pages[page]
	}
	if page+1 < len(f.pages) {
		out.NextToken = aws.String(pageToken(page + 1))
	}
	return out, nil
}

func (f *fakeSNS) Subscribe(_ context.Context, in *sns.SubscribeInput, _ ...func(*sns.Options)) (*sns.SubscribeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribes = append(f.subscribes, in)
	if len(f.subErrs) > 0 {
		err := f.subErrs[0]
		f.subErrs = f.subErrs[1:]
		return nil, err
	}
	return &sns.SubscribeOutput{SubscriptionArn: aws.String("pending confirmation")}, nil
}

func (f *fakeSNS) state() (lists int, subs []*sns.SubscribeInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, append([]*sns.SubscribeInput(nil), f.subscribes...)
}

func pageToken(i int) string { return "page-" + string(rune('a'+i)) }

func sub(endpoint, arn string) snstypes.Subscription {
	return snstypes.Subscription{Endpoint: aws.String(endpoint), SubscriptionArn: aws.String(arn), Protocol: aws.String("https")}
}

const hookEndpoint = testBaseURL + "/hooks/ses"

func runWorker(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := h.svc.SubscriberWorker()
	if w.Name() != events.SubscriberWorkerName {
		t.Fatalf("worker name = %q", w.Name())
	}
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("worker did not finish")
	}
}

func TestSubscriberWorker(t *testing.T) {
	confirmedARN := testTopicARN + ":0b1f7c1e"
	tests := []struct {
		name      string
		pages     [][]snstypes.Subscription
		listErrs  []error
		subErrs   []error
		wantSubs  int
		wantLists int
	}{
		{name: "not subscribed", pages: [][]snstypes.Subscription{{sub("https://other.example.org/hook", confirmedARN)}}, wantSubs: 1, wantLists: 1},
		{name: "empty topic", pages: nil, wantSubs: 1, wantLists: 1},
		{name: "already confirmed", pages: [][]snstypes.Subscription{{sub(hookEndpoint, confirmedARN)}}, wantSubs: 0, wantLists: 1},
		{name: "pending confirmation is subscribed again", pages: [][]snstypes.Subscription{{sub(hookEndpoint, "PendingConfirmation")}}, wantSubs: 1, wantLists: 1},
		{name: "confirmed on a later page", pages: [][]snstypes.Subscription{
			{sub("https://a.example.org/x", confirmedARN)},
			{sub("https://b.example.org/x", confirmedARN)},
			{sub(hookEndpoint, confirmedARN)},
		}, wantSubs: 0, wantLists: 3},
		{name: "list errors are retried", listErrs: []error{errBoom, errBoom}, wantSubs: 1, wantLists: 3},
		{name: "subscribe errors are retried", subErrs: []error{errBoom}, wantSubs: 2, wantLists: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.sns.pages, h.sns.listErrs, h.sns.subErrs = tt.pages, tt.listErrs, tt.subErrs
			runWorker(t, h)
			lists, subs := h.sns.state()
			if lists != tt.wantLists {
				t.Errorf("ListSubscriptionsByTopic calls = %d, want %d", lists, tt.wantLists)
			}
			if len(subs) != tt.wantSubs {
				t.Fatalf("Subscribe calls = %d, want %d", len(subs), tt.wantSubs)
			}
			for _, in := range subs {
				if aws.ToString(in.TopicArn) != testTopicARN || aws.ToString(in.Protocol) != "https" ||
					aws.ToString(in.Endpoint) != hookEndpoint || !in.ReturnSubscriptionArn {
					t.Errorf("Subscribe input = topic %q protocol %q endpoint %q return %v",
						aws.ToString(in.TopicArn), aws.ToString(in.Protocol), aws.ToString(in.Endpoint), in.ReturnSubscriptionArn)
				}
			}
			for _, topic := range h.sns.topics {
				if topic != testTopicARN {
					t.Errorf("listed topic %q", topic)
				}
			}
		})
	}
}

func TestSubscriberWorkerWithoutTopic(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) { d.Config.SNSTopicARN = "" })
	runWorker(t, h)
	if lists, subs := h.sns.state(); lists != 0 || len(subs) != 0 {
		t.Fatalf("SNS was called without a topic: lists %d, subscribes %d", lists, len(subs))
	}
}

func TestSubscriberWorkerWithoutClient(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) { d.SNS = nil })
	runWorker(t, h)
}

func TestSubscriberWorkerStopsOnCancel(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) {
		d.SubscribeMinBackoff = time.Hour
		d.SubscribeMaxBackoff = time.Hour
	})
	h.sns.listErrs = []error{errBoom, errBoom, errBoom}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.svc.SubscriberWorker().Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lists, _ := h.sns.state(); lists >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never listed subscriptions")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}
}

func TestEnsureSubscribedHTTPBaseURL(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) { d.Config.BaseURL = "http://mail.example.com:8080" })
	subscribed, err := h.svc.EnsureSubscribed(context.Background())
	if err != nil || !subscribed {
		t.Fatalf("EnsureSubscribed = %v, %v", subscribed, err)
	}
	_, subs := h.sns.state()
	if len(subs) != 1 || aws.ToString(subs[0].Protocol) != "http" || aws.ToString(subs[0].Endpoint) != "http://mail.example.com:8080/hooks/ses" {
		t.Fatalf("Subscribe = %+v", subs)
	}
}
