// Package portal serves the v154 native sent-mail cache API.
package portal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"rf4server/internal/store"
)

const nativeSentMailPageSize = 100

// These JSON contracts are recovered from the v154 launcher's Go type metadata.
type nativeSentMailRequest struct {
	Through   int64 `json:"through"`
	AfterPeer int64 `json:"afterPeer"`
	Peer      int64 `json:"peer"`
	Before    int64 `json:"before"`
	Read      int64 `json:"read"`
}

type nativeSentMailPage struct {
	Schema    int64                  `json:"schema"`
	Owner     int64                  `json:"owner"`
	Through   int64                  `json:"through"`
	AfterPeer int64                  `json:"afterPeer"`
	Peers     []uint64               `json:"peers"`
	MorePeers bool                   `json:"morePeers"`
	Peer      int64                  `json:"peer"`
	Before    int64                  `json:"before"`
	Read      int64                  `json:"read"`
	HasMore   bool                   `json:"hasMore"`
	Messages  []nativeSentMailRecord `json:"messages"`
}

type nativeSentMailRecord struct {
	Sequence  uint64             `json:"sequence"`
	Recipient uint64             `json:"recipient"`
	Item      nativeSentMailItem `json:"item"`
}

type nativeSentMailItem struct {
	Meta   nativeSentMailMeta `json:"meta"`
	Status uint32             `json:"status"`
}

type nativeSentMailMeta struct {
	ID      string                     `json:"id"`
	Created int64                      `json:"created"`
	Sender  int64                      `json:"sender"`
	Type    string                     `json:"type"`
	Text    string                     `json:"text"`
	Items   []nativeSentMailAttachment `json:"items"`
}

type nativeSentMailAttachment struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
	SysID string `json:"sysID"`
}

// nativeSentMail returns a stable snapshot of the authenticated player's
// outbound mail. With peer zero it pages the recipient list; with peer set it
// pages messages for that recipient. Incoming mail is never included.
func (s *Server) nativeSentMail(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.identity(w, r)
	if !ok {
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var request *nativeSentMailRequest
	if err := decoder.Decode(&request); err != nil || request == nil {
		fail(w, 400, "寄件记录请求无效")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		fail(w, 400, "寄件记录请求无效")
		return
	}
	if request.Through < 0 || request.AfterPeer < 0 || request.Peer < 0 ||
		request.Before < 0 || request.Read < 0 {
		fail(w, 400, "寄件记录游标无效")
		return
	}

	// Freeze the first page at the sender's current highest message ID. Later
	// pages carry the same through cursor so newly sent mail cannot shift pages.
	var maxID int64
	if err := s.db.SQL.QueryRowContext(r.Context(),
		`SELECT COALESCE(MAX(message_id),0) FROM sent_mail WHERE sender=?`, uid).Scan(&maxID); err != nil {
		fail(w, 500, "寄件记录暂时无法读取")
		return
	}
	through := maxID
	if request.Through > 0 && request.Through < through {
		through = request.Through
	}

	page := nativeSentMailPage{
		Schema: 1, Owner: uid, Through: through,
		AfterPeer: request.AfterPeer, Peers: make([]uint64, 0),
		Peer: request.Peer, Before: request.Before,
		Read: request.Read, Messages: make([]nativeSentMailRecord, 0),
	}

	if request.Peer == 0 {
		rows, err := s.db.SQL.QueryContext(r.Context(), `
			SELECT DISTINCT recipient
			FROM sent_mail
			WHERE sender=? AND message_id<=? AND recipient>?
			ORDER BY recipient
			LIMIT ?`, uid, through, request.AfterPeer, nativeSentMailPageSize+1)
		if err != nil {
			fail(w, 500, "寄件联系人暂时无法读取")
			return
		}
		peers := make([]uint64, 0)
		for rows.Next() {
			var recipient int64
			if err := rows.Scan(&recipient); err != nil {
				rows.Close()
				fail(w, 500, "寄件联系人暂时无法读取")
				return
			}
			peers = append(peers, uint64(recipient))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			fail(w, 500, "寄件联系人暂时无法读取")
			return
		}
		rows.Close()
		if len(peers) > nativeSentMailPageSize {
			page.MorePeers = true
			peers = peers[:nativeSentMailPageSize]
		}
		page.Peers = peers
		if len(peers) > 0 {
			page.AfterPeer = int64(peers[len(peers)-1])
		}
		write(w, page)
		return
	}

	var readCursor int64
	if err := s.db.SQL.QueryRowContext(r.Context(), `
		SELECT COALESCE(MAX(message_id),0)
		FROM sent_mail
		WHERE sender=? AND recipient=? AND message_id<=? AND read<>0`,
		uid, request.Peer, through).Scan(&readCursor); err != nil {
		fail(w, 500, "寄件已读状态暂时无法读取")
		return
	}
	page.Read = readCursor

	query := `
		SELECT message_id, recipient, sender, kind, COALESCE(text,''), COALESCE(items_json,''),
		       COALESCE(created_ts,0), read
		FROM sent_mail
		WHERE sender=? AND recipient=? AND message_id<=?`
	args := []any{uid, request.Peer, through}
	if request.Before > 0 {
		query += ` AND id<?`
		args = append(args, request.Before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, nativeSentMailPageSize+1)

	rows, err := s.db.SQL.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 500, "寄件记录暂时无法读取")
		return
	}
	var sent []*store.MailRow
	for rows.Next() {
		mail := &store.MailRow{}
		if err := rows.Scan(&mail.ID, &mail.UID, &mail.Sender, &mail.Kind, &mail.Text,
			&mail.ItemsJSON, &mail.CreatedTS, &mail.Read); err != nil {
			rows.Close()
			fail(w, 500, "寄件记录暂时无法读取")
			return
		}
		sent = append(sent, mail)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fail(w, 500, "寄件记录暂时无法读取")
		return
	}
	rows.Close()

	if len(sent) > nativeSentMailPageSize {
		page.HasMore = true
		sent = sent[:nativeSentMailPageSize]
	}
	for _, mail := range sent {
		attachments := make([]nativeSentMailAttachment, 0)
		if mail.ItemsJSON != "" {
			var stored []store.MailItem
			if err := json.Unmarshal([]byte(mail.ItemsJSON), &stored); err != nil {
				fail(w, 500, "寄件附件数据无法读取")
				return
			}
			for _, attachment := range stored {
				count := attachment.Qty
				if count < 1 {
					count = 1
				}
				attachments = append(attachments, nativeSentMailAttachment{
					ID: attachment.GUID, Count: count, SysID: attachment.ItemID,
				})
			}
		}
		// All player-to-player mails use MailboxItemTypes.Message in the game
		// protocol; attachments are represented by Meta.Items, not a new type.
		mailType := "message"
		status := uint32(0)
		if mail.Read != 0 {
			status = 1
		}
		page.Messages = append(page.Messages, nativeSentMailRecord{
			Sequence: uint64(mail.ID), Recipient: uint64(mail.UID),
			Item: nativeSentMailItem{
				Meta: nativeSentMailMeta{
					ID: fmt.Sprintf("%032x", uint64(mail.ID)),
					Created: int64(mail.CreatedTS), Sender: mail.Sender,
					Type: mailType, Text: mail.Text, Items: attachments,
				},
				Status: status,
			},
		})
	}
	if len(sent) > 0 {
		page.Before = sent[len(sent)-1].ID
	}
	write(w, page)
}
