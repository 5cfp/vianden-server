package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/files"
)

// Files is the upload logic the API needs. The real server passes a *files.Service.
type Files interface {
	Upload(ctx context.Context, by accounts.User, filename string, body io.Reader) (files.Attachment, error)
	Find(ctx context.Context, id int64) (files.Download, error)
	Open(d files.Download) (io.ReadSeekCloser, error)
}

type attachmentResponse struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"` // an image type, or application/octet-stream
	Size        int64  `json:"size"`
	Width       *int   `json:"width"` // images only, else null
	Height      *int   `json:"height"`
}

func toAttachmentResponse(a files.Attachment) attachmentResponse {
	r := attachmentResponse{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size}
	if a.IsImage() {
		r.Width, r.Height = &a.Width, &a.Height
	}
	return r
}

// uploadTimeout: how long a client may take to send a file (25 MB on a slow line).
// Without it, a client could hold a connection open forever by sending very slowly.
const uploadTimeout = 5 * time.Minute

func handleUpload(svc Files, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
		// MaxBytesReader also closes the connection after the limit, so the rest of a
		// huge upload is not even read.
		body := http.MaxBytesReader(w, r.Body, files.MaxSize+1)

		a, err := svc.Upload(r.Context(), s.User, r.URL.Query().Get("filename"), body)
		switch {
		case err == nil:
			logger.Info("file uploaded", "attachment_id", a.ID, "by_user_id", s.User.ID, "size", a.Size, "type", a.ContentType)
			writeJSON(w, http.StatusCreated, struct {
				Attachment attachmentResponse `json:"attachment"`
			}{toAttachmentResponse(a)})
		case errors.Is(err, files.ErrTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "files can be at most 25 MB")
		case errors.Is(err, files.ErrTooManyPending):
			writeError(w, http.StatusTooManyRequests, "too_many_uploads", err.Error())
		default:
			writeServiceError(w, logger, "upload", err)
		}
	}
}

func handleDownload(svc Files, chatSvc Chat, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		notFound := func() { writeError(w, http.StatusNotFound, "not_found", files.ErrNotFound.Error()) }
		id, ok := pathID(r, "id")
		if !ok {
			notFound()
			return
		}
		d, err := svc.Find(r.Context(), id)
		if errors.Is(err, files.ErrNotFound) {
			notFound()
			return
		}
		if err != nil {
			writeServiceError(w, logger, "find attachment", err)
			return
		}
		// Who may download: before it is sent, only its uploader. After, everyone who can
		// see the channel of its message. Otherwise 404, so ids reveal nothing.
		if d.ChannelID == 0 {
			if d.UploaderID != s.User.ID {
				notFound()
				return
			}
		} else if _, err := chatSvc.Channel(r.Context(), s.User, d.ChannelID); err != nil {
			notFound()
			return
		}

		f, err := svc.Open(d)
		if err != nil {
			logger.Error("open attachment failed", "attachment_id", d.ID, "error", err)
			notFound()
			return
		}
		defer f.Close()

		h := w.Header()
		// The type the SERVER decided (never the uploader's claim), and no guessing by the
		// browser (nosniff). The CSP sandbox stops a file from running scripts or loading
		// anything, even if someone opens the URL directly in a browser.
		h.Set("Content-Type", d.ContentType)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		disposition := "attachment" // non-images: always "save as", never displayed
		if d.IsImage() {
			disposition = "inline"
		}
		h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": d.Filename}))
		// A file never changes, so clients may keep it. "private": shared caches (proxies)
		// must not, because downloads need a login.
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
		// ServeContent handles Range requests (resuming downloads) and HEAD.
		http.ServeContent(w, r, "", time.Time{}, f)
	}
}
