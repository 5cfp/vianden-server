package chat

import (
	"context"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/db"
	"github.com/5cfp/vianden-server/internal/files"
	"github.com/5cfp/vianden-server/internal/perm"
)

// maxMentions caps how many people one message can ping (stops "mention spam").
const maxMentions = 50

// mentionPattern finds "@name". The character before "@" must not be a letter, digit, or
// one of "_.-@", so e-mail addresses like "sara@example.com" are not mentions.
var mentionPattern = regexp.MustCompile(`(?:^|[^\pL\pN_.@-])@([A-Za-z0-9][A-Za-z0-9_.-]*)`)

// parseMentions returns the usernames written as "@name" in a message (lowercase, without
// duplicates) and whether it contains "@everyone". A name followed by punctuation ("@sara.")
// is tried both with and without the trailing ".", "-", "_", because usernames may contain
// those characters too.
func parseMentions(content string) (usernames []string, everyone bool) {
	seen := map[string]bool{}
	for _, m := range mentionPattern.FindAllStringSubmatch(content, -1) {
		name := strings.ToLower(m[1])
		for _, candidate := range []string{name, strings.TrimRight(name, "._-")} {
			if candidate == "everyone" {
				everyone = true
				continue
			}
			if len(candidate) >= 3 && len(candidate) <= 32 && !seen[candidate] && len(seen) < maxMentions {
				seen[candidate] = true
				usernames = append(usernames, candidate)
			}
		}
	}
	return usernames, everyone
}

// saveMentions stores who a message pings: the users it names, plus the author of the
// message it replies to (replyAuthor, 0 if none). Only users who can SEE the channel are
// pinged, and never the author themselves. Runs inside the caller's transaction (q).
func saveMentions(ctx context.Context, q *db.Queries, messageID int64, by accounts.User, ch Channel, usernames []string, replyAuthor int64) error {
	if len(usernames) == 0 && replyAuthor == 0 {
		return nil
	}
	if usernames == nil {
		usernames = []string{} // an empty array, not NULL
	}
	rows, err := q.MentionCandidates(ctx, db.MentionCandidatesParams{Usernames: usernames, ReplyAuthorID: replyAuthor})
	if err != nil {
		return err
	}
	var ids []int64
	for _, r := range rows {
		if r.ID != by.ID && ch.CanView(perm.Role(r.Role)) && len(ids) < maxMentions {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return q.AddMentions(ctx, db.AddMentionsParams{MessageID: messageID, UserIds: ids})
}

// canMentionEveryone: "@everyone" only pings when the author is allowed to use it;
// otherwise it is just text.
func canMentionEveryone(by accounts.User, everyone bool) bool {
	return everyone && by.Role.Has(perm.MentionEveryone)
}

// loadMentions fills in Message.Mentions for a list of messages (one query for all).
func (s *Service) loadMentions(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, len(msgs))
	index := make(map[int64]int, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
		index[m.ID] = i
	}
	rows, err := s.queries.ListMentions(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		i := index[r.MessageID]
		msgs[i].Mentions = append(msgs[i].Mentions, Author{ID: r.ID, Username: r.Username, DisplayName: r.DisplayName})
	}
	return nil
}

// withTx runs fn in a database transaction: everything in it is saved together, or (if fn
// returns an error) nothing is.
func (s *Service) withTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(s.queries.WithTx(tx))
	})
}

// decorate fills in what a message object needs from other tables: mentions and files.
func (s *Service) decorate(ctx context.Context, msgs []Message) error {
	if err := s.loadMentions(ctx, msgs); err != nil {
		return err
	}
	return s.loadAttachments(ctx, msgs)
}

// loadAttachments fills in Message.Attachments (one query for all messages).
func (s *Service) loadAttachments(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, len(msgs))
	index := make(map[int64]int, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
		index[m.ID] = i
	}
	rows, err := s.queries.ListAttachments(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.MessageID == nil {
			continue
		}
		i := index[*r.MessageID]
		msgs[i].Attachments = append(msgs[i].Attachments, files.Attachment{
			ID: r.ID, Filename: r.Filename, ContentType: r.ContentType, Size: r.Size,
			Width: int(r.Width.Int32), Height: int(r.Height.Int32),
		})
	}
	return nil
}
