package xiaohongshu

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommentTimeoutPreservesLastCheckpoint(t *testing.T) {
	result := &FeedDetailResponse{Note: FeedDetail{NoteID: "initial"}}
	got, err := finishCommentLoad(context.Background(), &result, func() error {
		result = &FeedDetailResponse{Note: FeedDetail{NoteID: "checkpoint"}}
		panic(context.DeadlineExceeded)
	}, func() (*FeedDetailResponse, error) { return nil, context.DeadlineExceeded })
	if err != nil || got == nil || got.Note.NoteID != "checkpoint" || got.CommentLoadWarning == "" {
		t.Fatalf("lost partial result or warning: got=%+v err=%v", got, err)
	}
}

func TestCommentFailureCanReadFinalSnapshot(t *testing.T) {
	result := &FeedDetailResponse{}
	got, err := finishCommentLoad(context.Background(), &result,
		func() error { return context.DeadlineExceeded },
		func() (*FeedDetailResponse, error) {
			return &FeedDetailResponse{Note: FeedDetail{NoteID: "latest"}}, nil
		})
	if err != nil || got.Note.NoteID != "latest" || got.CommentLoadWarning == "" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestCallerCancellationIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	result := &FeedDetailResponse{}
	got, err := finishCommentLoad(ctx, &result, func() error { cancel(); return ctx.Err() },
		func() (*FeedDetailResponse, error) { t.Fatal("must not read cancelled browser"); return nil, nil })
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestSuccessfulCommentLoadHasNoWarning(t *testing.T) {
	result := &FeedDetailResponse{}
	got, err := finishCommentLoad(context.Background(), &result, func() error { return nil },
		func() (*FeedDetailResponse, error) {
			return &FeedDetailResponse{Note: FeedDetail{NoteID: "complete"}}, nil
		})
	if err != nil || got.Note.NoteID != "complete" || got.CommentLoadWarning != "" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestCommentLoadTimeoutScalesAndCaps(t *testing.T) {
	tests := []struct {
		items int
		want  time.Duration
	}{
		{items: 0, want: 40 * time.Second},
		{items: 20, want: 40 * time.Second},
		{items: 21, want: 52 * time.Second},
		{items: 30, want: 52 * time.Second},
		{items: 31, want: 64 * time.Second},
		{items: 50, want: 75 * time.Second},
		{items: 100, want: 75 * time.Second},
	}

	for _, tt := range tests {
		if got := commentLoadTimeout(tt.items); got != tt.want {
			t.Errorf("commentLoadTimeout(%d) = %s, want %s", tt.items, got, tt.want)
		}
	}
}
