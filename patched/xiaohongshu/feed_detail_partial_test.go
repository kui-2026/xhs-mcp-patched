package xiaohongshu

import (
	"context"
	"errors"
	"testing"
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
