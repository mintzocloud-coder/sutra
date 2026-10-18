// SPDX-License-Identifier: AGPL-3.0-or-later
// Package media owns the media registry: user-uploaded videos and images.
//
// Data path (bytes) NEVER touches the API. The flow is:
//  1. POST /v1/media/upload → the API asks the SeaweedFS master for a
//     file id (dir/assign), records a pending media row, and returns a
//     direct PUT URL to the client. ("Presigned" here means
//     pre-assigned: unlike S3 presigned URLs there is no signature or
//     expiry — security comes from unguessable fids, the status gate
//     below, and the network boundary. This is documented for clients.)
//  2. The client PUTs bytes straight to SeaweedFS.
//  3. POST /v1/media/{id}/complete → the API HEADs the fid to confirm
//     bytes landed, flips status, and enqueues videos onto the Valkey
//     transcode stream the D4-part-2 worker drains. Images are ready
//     at this point (no transcode needed).
//  4. Videos become ready when the worker finishes (poster + HLS
//     renditions); failures land in status=failed with error set.
//
// Posts may only reference READY media (see CheckReady) — the gate
// posts.Create applies via its MediaReady hook.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Media kinds and lifecycle statuses. Statuses mirror the migration
// 0005 CHECK constraint; keep them in sync.
const (
	KindVideo = "video"
	KindImage = "image"

	StatusPending  = "pending"  // upload URL minted, no bytes yet
	StatusUploaded = "uploaded" // bytes confirmed; videos await transcode
	StatusReady    = "ready"    // safe to reference from posts
	StatusFailed   = "failed"   // worker gave up; see Error
)

// Limits.
const (
	MaxFilenameLen = 255 // bytes
	MaxMimeLen     = 128 // bytes
)

// TranscodeStream is the Valkey stream the complete endpoint XADDs video
// jobs onto for the D4-part-2 transcode worker.
const TranscodeStream = "media:transcode"

var (
	// ErrUnknownMedia is returned when a media id was never minted.
	ErrUnknownMedia = errors.New("media: unknown media id")
	// ErrMediaNotReady is returned when media exists but is not ready.
	ErrMediaNotReady = errors.New("media: media is not ready")
	// ErrMediaNotOwned is returned when media belongs to another user.
	ErrMediaNotOwned = errors.New("media: media belongs to another user")
	// ErrMediaNotFound is returned by Get/Complete for unknown ids.
	ErrMediaNotFound = errors.New("media: media not found")
	// ErrBadKind is returned for kinds other than video/image.
	ErrBadKind = errors.New("media: kind must be 'video' or 'image'")
	// ErrBadMime is returned when the MIME type does not match the kind.
	ErrBadMime = errors.New("media: mime type must match the media kind")
	// ErrBadFilename is returned for empty/oversized filenames.
	ErrBadFilename = errors.New("media: filename must be 1-255 bytes with no path separators")
	// ErrAlreadyCompleted is returned when completing non-pending media.
	ErrAlreadyCompleted = errors.New("media: upload already completed")
	// ErrBytesMissing is returned when SeaweedFS has no bytes for the fid.
	ErrBytesMissing = errors.New("media: no bytes found on the upload URL")
	// ErrMasterUnreachable is returned when the SeaweedFS master cannot
	// be reached for dir/assign.
	ErrMasterUnreachable = errors.New("media: storage backend temporarily unavailable")
)

// Media is the API representation of one media row.
type Media struct {
	ID          string      `json:"id"`
	OwnerID     string      `json:"owner_id"`
	Kind        string      `json:"kind"`
	Status      string      `json:"status"`
	Filename    string      `json:"filename,omitempty"`
	MimeType    string      `json:"mime_type,omitempty"`
	SizeBytes   *int64      `json:"size_bytes,omitempty"`
	Width       *int        `json:"width,omitempty"`
	Height      *int        `json:"height,omitempty"`
	DurationMs  *int64      `json:"duration_ms,omitempty"`
	PosterURL   string      `json:"poster_url,omitempty"`
	Renditions  []Rendition `json:"renditions"`
	PlaybackURL string      `json:"playback_url,omitempty"`
	Error       string      `json:"error,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Rendition is one HLS rendition the worker produced (video only).
type Rendition struct {
	Name      string `json:"name"`      // e.g. "720p"
	URL       string `json:"url"`       // HLS playlist URL
	Bandwidth int64  `json:"bandwidth"` // bits/sec
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// UploadSession is what POST /v1/media/upload returns: the minted
// media id plus the direct SeaweedFS PUT URL.
type UploadSession struct {
	MediaID   string `json:"media_id"`
	UploadURL string `json:"upload_url"`
	// Method is always PUT: clients upload bytes with a single PUT to
	// UploadURL (Content-Type = the declared mime type).
	Method string `json:"method"`
}

// Service is the media registry. masterAddr is the SeaweedFS master
// (host:port, e.g. "seaweedfs:9333") used for dir/assign; publicBaseURL
// is the SeaweedFS volume base URL clients can actually reach
// (e.g. "http://localhost:8080") that upload/download URLs are built
// from. rdb feeds the transcode stream; it may be nil in unit tests
// (Complete then skips the enqueue and the worker picks the row up on
// its sweep instead).
type Service struct {
	pool          *db.Pool
	rdb           *redis.Client
	masterAddr    string
	publicBaseURL string
	http          *http.Client
}

// NewService builds the media registry.
func NewService(pool *db.Pool, rdb *redis.Client, masterAddr, publicBaseURL string) *Service {
	return &Service{
		pool:          pool,
		rdb:           rdb,
		masterAddr:    strings.TrimSuffix(masterAddr, "/"),
		publicBaseURL: strings.TrimSuffix(publicBaseURL, "/"),
		http:          &http.Client{Timeout: 10 * time.Second},
	}
}

// validateUpload checks kind/mime/filename before any SeaweedFS call.
func validateUpload(kind, mime, filename string) error {
	if kind != KindVideo && kind != KindImage {
		return ErrBadKind
	}
	mime = strings.TrimSpace(mime)
	if mime == "" || len(mime) > MaxMimeLen || !strings.HasPrefix(mime, kind+"/") {
		return ErrBadMime
	}
	if len(filename) == 0 || len(filename) > MaxFilenameLen ||
		strings.ContainsAny(filename, "/\\") {
		return ErrBadFilename
	}
	return nil
}

// assignResponse is the SeaweedFS master's /dir/assign reply.
type assignResponse struct {
	Fid string `json:"fid"`
	URL string `json:"url"`
}

// PingMaster checks the SeaweedFS master's /cluster/status. Used at
// startup for an honest log line (unreachable = warn, not fatal).
func PingMaster(ctx context.Context, masterAddr string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+strings.TrimSuffix(masterAddr, "/")+"/cluster/status", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("master status %d", resp.StatusCode)
	}
	return nil
}

// assign asks the SeaweedFS master for a fresh file id.
func (s *Service) assign(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+s.masterAddr+"/dir/assign", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMasterUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("%w: master status %d (%s)",
			ErrMasterUnreachable, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ar assignResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", fmt.Errorf("media: decode dir/assign: %w", err)
	}
	if ar.Fid == "" {
		return "", fmt.Errorf("%w: master returned no fid", ErrMasterUnreachable)
	}
	return ar.Fid, nil
}

// CreateUpload mints a media row and returns a direct PUT URL for the
// client. The row starts pending; bytes arrive out-of-band via the PUT.
func (s *Service) CreateUpload(ctx context.Context, ownerID pgtype.UUID, kind, filename, mime string) (*UploadSession, error) {
	if err := validateUpload(kind, mime, filename); err != nil {
		return nil, err
	}
	fid, err := s.assign(ctx)
	if err != nil {
		return nil, err
	}
	var id pgtype.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO media (owner_id, kind, fid, filename, mime_type, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		ownerID, kind, fid, filename, mime, StatusPending).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &UploadSession{
		MediaID:   id.String(),
		UploadURL: s.publicBaseURL + "/" + fid,
		Method:    http.MethodPut,
	}, nil
}

// scanMedia maps one media row. fid stays internal; public URLs are
// derived from it (never expose the raw fid — it is a storage handle).
func (s *Service) scanMedia(row pgx.Row) (*Media, error) {
	var (
		m              Media
		id             pgtype.UUID
		owner          pgtype.UUID
		fid            *string
		posterFid      *string
		filename       *string
		mimeType       *string
		errorText      *string
		renditionsJSON []byte
	)
	if err := row.Scan(&id, &owner, &m.Kind, &m.Status, &fid, &filename,
		&mimeType, &m.SizeBytes, &m.Width, &m.Height, &m.DurationMs,
		&posterFid, &renditionsJSON, &errorText, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.ID = id.String()
	m.OwnerID = owner.String()
	if fid != nil {
		m.PlaybackURL = s.publicBaseURL + "/" + *fid
	}
	if filename != nil {
		m.Filename = *filename
	}
	if mimeType != nil {
		m.MimeType = *mimeType
	}
	if errorText != nil {
		m.Error = *errorText
	}
	if posterFid != nil {
		m.PosterURL = s.publicBaseURL + "/" + *posterFid
	}
	m.Renditions = []Rendition{}
	if len(renditionsJSON) > 0 {
		var rs []Rendition
		if err := json.Unmarshal(renditionsJSON, &rs); err == nil && rs != nil {
			m.Renditions = rs
		}
	}
	return &m, nil
}

const mediaColumns = `id, owner_id, kind, status, fid, filename, mime_type,
	size_bytes, width, height, duration_ms, poster_fid, renditions, error,
	created_at, updated_at`

// Get returns one media row by id. Unknown ids are ErrMediaNotFound.
func (s *Service) Get(ctx context.Context, id pgtype.UUID) (*Media, error) {
	m, err := s.scanMedia(s.pool.QueryRow(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMediaNotFound
	}
	return m, err
}

// Complete marks a pending upload done. The client calls this after its
// PUT to the upload URL: the API HEADs the fid on SeaweedFS to confirm
// bytes actually landed (size > 0), records size_bytes, and advances
// status — images straight to ready (nothing to transcode), videos to
// uploaded + enqueued for the transcode worker. Re-completing is a 409:
// the client already got its answer once.
func (s *Service) Complete(ctx context.Context, id, callerID pgtype.UUID) (*Media, error) {
	var (
		owner  pgtype.UUID
		fid    string
		kind   string
		status string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT owner_id, fid, kind, status FROM media WHERE id = $1`, id).
		Scan(&owner, &fid, &kind, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMediaNotFound
	}
	if err != nil {
		return nil, err
	}
	if owner != callerID {
		return nil, ErrMediaNotOwned
	}
	if status != StatusPending {
		return nil, ErrAlreadyCompleted
	}

	// Confirm bytes: HEAD the fid through the public base. SeaweedFS
	// answers 200 with Content-Length for stored files, 404 for missing.
	size, err := s.headSize(ctx, fid)
	if err != nil {
		return nil, err
	}

	next := StatusUploaded
	if kind == KindImage {
		next = StatusReady
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE media SET status = $2, size_bytes = $3 WHERE id = $1`,
		id, next, size); err != nil {
		return nil, err
	}

	if kind == KindVideo && s.rdb != nil {
		// Best-effort enqueue: the worker also sweeps uploaded rows,
		// so a lost XADD only delays, never loses, the job.
		_ = s.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: TranscodeStream,
			Values: map[string]any{"media_id": id.String()},
		}).Err()
	}
	return s.Get(ctx, id)
}

// headSize HEADs the fid and returns its Content-Length. 404 (or any
// non-200) means the bytes never arrived → ErrBytesMissing.
func (s *Service) headSize(ctx context.Context, fid string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		s.publicBaseURL+"/"+fid, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrMasterUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, ErrBytesMissing
	}
	if resp.ContentLength <= 0 {
		return 0, ErrBytesMissing
	}
	return resp.ContentLength, nil
}

// CheckReady is the posts gate: every id must name an existing media
// row owned by ownerID in ready status. Shape-valid ids that were never
// minted are ErrUnknownMedia; existing-but-unready is ErrMediaNotReady;
// someone else's media is ErrMediaNotOwned. One query for the whole
// list (posts cap at 10 attachments).
func (s *Service) CheckReady(ctx context.Context, ownerID pgtype.UUID, mediaIDs []string) error {
	if len(mediaIDs) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_id, status FROM media WHERE id = ANY($1)`, mediaIDs)
	if err != nil {
		return err
	}
	type row struct {
		owner  pgtype.UUID
		status string
	}
	seen := make(map[string]row, len(mediaIDs))
	for rows.Next() {
		var (
			id     pgtype.UUID
			owner  pgtype.UUID
			status string
		)
		if err := rows.Scan(&id, &owner, &status); err != nil {
			rows.Close()
			return err
		}
		seen[id.String()] = row{owner: owner, status: status}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, mid := range mediaIDs {
		r, ok := seen[mid]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownMedia, mid)
		}
		if r.owner != ownerID {
			return fmt.Errorf("%w: %s", ErrMediaNotOwned, mid)
		}
		if r.status != StatusReady {
			return fmt.Errorf("%w: %s is %s", ErrMediaNotReady, mid, r.status)
		}
	}
	return nil
}

// MarkReady is the transcode worker's (D4 part 2) success path: stores
// the poster fid, HLS renditions, dimensions and duration, and flips
// the row to ready. Kept here (not in the worker) so the status
// transition lives next to the rest of the lifecycle.
func (s *Service) MarkReady(ctx context.Context, id pgtype.UUID, width, height int, durationMs int64, posterFID string, renditions []Rendition) error {
	rj, err := json.Marshal(renditions)
	if err != nil {
		return err
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE media
		SET status = $2, width = $3, height = $4, duration_ms = $5,
		    poster_fid = $6, renditions = $7, error = NULL
		WHERE id = $1 AND status IN ('uploaded', 'failed')`,
		id, StatusReady, width, height, durationMs, posterFID, rj)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrMediaNotFound
	}
	return nil
}

// MarkFailed is the transcode worker's (D4 part 2) give-up path:
// records the reason and parks the row in failed.
func (s *Service) MarkFailed(ctx context.Context, id pgtype.UUID, reason string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE media SET status = 'failed', error = $2
		WHERE id = $1 AND status IN ('uploaded', 'failed')`,
		id, reason)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrMediaNotFound
	}
	return nil
}
