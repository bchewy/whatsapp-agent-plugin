package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"bytes"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func debugContentLoggingEnabled() bool {
	return os.Getenv("WHATSAPP_MCP_DEBUG_CONTENT") == "1"
}

const bridgeTokenHeader = "X-WhatsApp-Bridge-Token"

func bridgeToken() string {
	return strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_TOKEN"))
}

func authorizeBridgeRequest(w http.ResponseWriter, r *http.Request, expectedToken string) bool {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(bridgeTokenHeader)), []byte(expectedToken)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// storeDir returns the directory the bridge should use for SQLite databases
// and downloaded media. It honors WHATSAPP_MCP_STORE_DIR so the store can live
// outside the cached plugin install directory and survive plugin upgrades.
func storeDir() string {
	if v := os.Getenv("WHATSAPP_MCP_STORE_DIR"); v != "" {
		return v
	}
	return "store"
}

// bridgePort returns the TCP port the local REST API should bind to. Defaults
// to 8080 and accepts an override via WHATSAPP_BRIDGE_PORT.
func bridgePort() int {
	if v := os.Getenv("WHATSAPP_BRIDGE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return 8080
}

func sanitizeMediaFilename(filename, mediaType string) string {
	filename = strings.TrimSpace(filepath.Base(strings.ReplaceAll(filename, "\\", "/")))
	if filename == "" || filename == "." || filename == ".." {
		prefix := mediaType
		if prefix == "" {
			prefix = "media"
		}
		return prefix + "_" + time.Now().Format("20060102_150405")
	}
	return filename
}

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

type EventInfo struct {
	Name               string
	Description        string
	LocationName       string
	LocationAddress    string
	Latitude           *float64
	Longitude          *float64
	JoinLink           string
	StartTime          int64
	EndTime            int64
	IsCanceled         bool
	ExtraGuestsAllowed bool
	IsScheduleCall     bool
	HasReminder        bool
	ReminderOffsetSec  int64
}

type HistoryAnchor struct {
	ID        string
	Timestamp time.Time
	IsFromMe  bool
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	dir := storeDir()
	// Create directory for database if it doesn't exist (owner-only).
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(dir, "messages.db"))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE TABLE IF NOT EXISTS events (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			name TEXT,
			description TEXT,
			location_name TEXT,
			location_address TEXT,
			latitude REAL,
			longitude REAL,
			join_link TEXT,
			start_time INTEGER,
			end_time INTEGER,
			is_canceled BOOLEAN,
			extra_guests_allowed BOOLEAN,
			is_schedule_call BOOLEAN,
			has_reminder BOOLEAN,
			reminder_offset_sec INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

func (store *MessageStore) StoreEvent(id, chatJID, sender string, timestamp time.Time, isFromMe bool, event EventInfo) error {
	var latitude, longitude interface{}
	if event.Latitude != nil {
		latitude = *event.Latitude
	}
	if event.Longitude != nil {
		longitude = *event.Longitude
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO events
		(id, chat_jid, sender, timestamp, is_from_me, name, description, location_name, location_address, latitude, longitude, join_link, start_time, end_time, is_canceled, extra_guests_allowed, is_schedule_call, has_reminder, reminder_offset_sec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, timestamp, isFromMe, event.Name, event.Description, event.LocationName, event.LocationAddress, latitude, longitude, event.JoinLink, event.StartTime, event.EndTime, event.IsCanceled, event.ExtraGuestsAllowed, event.IsScheduleCall, event.HasReminder, event.ReminderOffsetSec,
	)
	return err
}

func (store *MessageStore) GetOldestMessageAnchor(chatJID string) (*HistoryAnchor, error) {
	row := store.db.QueryRow(
		`SELECT id, timestamp, is_from_me
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp ASC
		LIMIT 1`,
		chatJID,
	)

	var anchor HistoryAnchor
	if err := row.Scan(&anchor.ID, &anchor.Timestamp, &anchor.IsFromMe); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &anchor, nil
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

func describeMessageShape(msg *waProto.Message) string {
	if msg == nil {
		return "nil"
	}

	fields := make([]string, 0, 8)
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		fields = append(fields, string(fd.Name()))
		return true
	})
	if len(fields) == 0 {
		return "empty"
	}

	details := make([]string, 0, 6)
	if protocol := msg.GetProtocolMessage(); protocol != nil {
		details = append(details, fmt.Sprintf("protocol_type=%s", protocol.GetType().String()))
		if nested := describeMessageShape(protocol.GetEditedMessage()); nested != "nil" {
			details = append(details, "edited="+nested)
		}
	}
	if ephemeral := msg.GetEphemeralMessage(); ephemeral != nil {
		details = append(details, "ephemeral_inner="+describeMessageShape(ephemeral.GetMessage()))
	}
	if viewOnce := msg.GetViewOnceMessage(); viewOnce != nil {
		details = append(details, "view_once_inner="+describeMessageShape(viewOnce.GetMessage()))
	}
	if viewOnceV2 := msg.GetViewOnceMessageV2(); viewOnceV2 != nil {
		details = append(details, "view_once_v2_inner="+describeMessageShape(viewOnceV2.GetMessage()))
	}
	if docCaption := msg.GetDocumentWithCaptionMessage(); docCaption != nil {
		details = append(details, "document_caption_inner="+describeMessageShape(docCaption.GetMessage()))
	}
	if edited := msg.GetEditedMessage(); edited != nil {
		details = append(details, "edited_inner="+describeMessageShape(edited.GetMessage()))
	}

	if len(details) == 0 {
		return strings.Join(fields, ",")
	}
	return strings.Join(fields, ",") + " [" + strings.Join(details, "; ") + "]"
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type SentMessageInfo struct {
	ID            string
	ChatJID       string
	Sender        string
	Content       string
	Timestamp     time.Time
	IsFromMe      bool
	MediaType     string
	Filename      string
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

type GroupParticipantResult struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	LID          string `json:"lid,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	DisplayName  string `json:"display_name,omitempty"`
	Error        int    `json:"error,omitempty"`
}

type AddGroupParticipantsRequest struct {
	GroupJID     string   `json:"group_jid"`
	Participants []string `json:"participants"`
}

type AddGroupParticipantsResponse struct {
	Success      bool                     `json:"success"`
	Message      string                   `json:"message"`
	Participants []GroupParticipantResult `json:"participants,omitempty"`
}

type RemoveGroupParticipantsRequest struct {
	GroupJID     string   `json:"group_jid"`
	Participants []string `json:"participants"`
}

type RemoveGroupParticipantsResponse struct {
	Success      bool                     `json:"success"`
	Message      string                   `json:"message"`
	Participants []GroupParticipantResult `json:"participants,omitempty"`
}

type LeaveGroupRequest struct {
	GroupJID string `json:"group_jid"`
}

type LeaveGroupResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type GroupInfoRequest struct {
	GroupJID string `json:"group_jid"`
}

type GroupInfoResponse struct {
	Success          bool                     `json:"success"`
	Message          string                   `json:"message"`
	GroupJID         string                   `json:"group_jid,omitempty"`
	Name             string                   `json:"name,omitempty"`
	MemberAddMode    string                   `json:"member_add_mode,omitempty"`
	ParticipantCount int                      `json:"participant_count,omitempty"`
	OwnParticipant   *GroupParticipantResult  `json:"own_participant,omitempty"`
	Participants     []GroupParticipantResult `json:"participants,omitempty"`
}

type GroupInviteLinkRequest struct {
	GroupJID string `json:"group_jid"`
	Reset    bool   `json:"reset,omitempty"`
}

type GroupInviteLinkResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	InviteLink string `json:"invite_link,omitempty"`
}

type CreateGroupRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
}

type CreateGroupResponse struct {
	Success      bool                     `json:"success"`
	Message      string                   `json:"message"`
	GroupJID     string                   `json:"group_jid,omitempty"`
	Participants []GroupParticipantResult `json:"participants,omitempty"`
}

func parseUserJID(ref string) (types.JID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return types.JID{}, fmt.Errorf("participant cannot be empty")
	}
	if strings.Contains(ref, "@") {
		return types.ParseJID(ref)
	}
	return types.JID{
		User:   ref,
		Server: types.DefaultUserServer,
	}, nil
}

func participantResult(participant types.GroupParticipant) GroupParticipantResult {
	result := GroupParticipantResult{
		JID:          participant.JID.String(),
		IsAdmin:      participant.IsAdmin,
		IsSuperAdmin: participant.IsSuperAdmin,
		DisplayName:  participant.DisplayName,
		Error:        participant.Error,
	}
	if !participant.PhoneNumber.IsEmpty() {
		result.PhoneNumber = participant.PhoneNumber.String()
	}
	if !participant.LID.IsEmpty() {
		result.LID = participant.LID.String()
	}
	return result
}

func parseParticipantRefs(participantRefs []string) ([]types.JID, error) {
	if len(participantRefs) == 0 {
		return nil, fmt.Errorf("at least one participant must be provided")
	}

	participants := make([]types.JID, 0, len(participantRefs))
	for _, ref := range participantRefs {
		participant, err := parseUserJID(ref)
		if err != nil {
			return nil, fmt.Errorf("error parsing participant %q: %v", ref, err)
		}
		if participant.Server != types.DefaultUserServer && participant.Server != types.HiddenUserServer {
			return nil, fmt.Errorf("participant %q must be a user JID or phone number", ref)
		}
		participants = append(participants, participant)
	}
	return participants, nil
}

func createGroup(client *whatsmeow.Client, name string, participantRefs []string) (bool, string, string, []GroupParticipantResult) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", "", nil
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return false, "Group name must be provided", "", nil
	}
	if len([]rune(name)) > 25 {
		return false, "Group names are limited to 25 characters", "", nil
	}

	participants, err := parseParticipantRefs(participantRefs)
	if err != nil {
		return false, err.Error(), "", nil
	}

	info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		return false, fmt.Sprintf("Failed to create group: %v", err), "", nil
	}

	results := make([]GroupParticipantResult, len(info.Participants))
	hasParticipantError := false
	for i, participant := range info.Participants {
		results[i] = participantResult(participant)
		if participant.Error != 0 {
			hasParticipantError = true
		}
	}

	if hasParticipantError {
		return false, "WhatsApp created the group but returned participant-level errors", info.JID.String(), results
	}
	return true, "Group created successfully", info.JID.String(), results
}

func addGroupParticipants(client *whatsmeow.Client, groupJID string, participantRefs []string) (bool, string, []GroupParticipantResult) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", nil
	}

	group, err := types.ParseJID(strings.TrimSpace(groupJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing group JID: %v", err), nil
	}
	if group.Server != types.GroupServer {
		return false, "group_jid must be a WhatsApp group JID ending in @g.us", nil
	}
	if len(participantRefs) == 0 {
		return false, "At least one participant must be provided", nil
	}

	participants, err := parseParticipantRefs(participantRefs)
	if err != nil {
		return false, err.Error(), nil
	}

	updated, err := client.UpdateGroupParticipants(context.Background(), group, participants, whatsmeow.ParticipantChangeAdd)
	if err != nil {
		return false, fmt.Sprintf("Failed to add group participants: %v", err), nil
	}

	results := make([]GroupParticipantResult, len(updated))
	hasParticipantError := false
	for i, participant := range updated {
		results[i] = participantResult(participant)
		if participant.Error != 0 {
			hasParticipantError = true
		}
	}

	if hasParticipantError {
		return false, "WhatsApp returned participant-level errors while adding one or more users", results
	}
	return true, "Group participants added successfully", results
}

func removeGroupParticipants(client *whatsmeow.Client, groupJID string, participantRefs []string) (bool, string, []GroupParticipantResult) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", nil
	}

	group, err := types.ParseJID(strings.TrimSpace(groupJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing group JID: %v", err), nil
	}
	if group.Server != types.GroupServer {
		return false, "group_jid must be a WhatsApp group JID ending in @g.us", nil
	}

	participants, err := parseParticipantRefs(participantRefs)
	if err != nil {
		return false, err.Error(), nil
	}

	updated, err := client.UpdateGroupParticipants(context.Background(), group, participants, whatsmeow.ParticipantChangeRemove)
	if err != nil {
		return false, fmt.Sprintf("Failed to remove group participants: %v", err), nil
	}

	results := make([]GroupParticipantResult, len(updated))
	hasParticipantError := false
	for i, participant := range updated {
		results[i] = participantResult(participant)
		if participant.Error != 0 {
			hasParticipantError = true
		}
	}

	if hasParticipantError {
		return false, "WhatsApp returned participant-level errors while removing one or more users", results
	}
	return true, "Group participants removed successfully", results
}

func leaveGroup(client *whatsmeow.Client, groupJID string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	group, err := types.ParseJID(strings.TrimSpace(groupJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing group JID: %v", err)
	}
	if group.Server != types.GroupServer {
		return false, "group_jid must be a WhatsApp group JID ending in @g.us"
	}

	if err := client.LeaveGroup(context.Background(), group); err != nil {
		return false, fmt.Sprintf("Failed to leave group: %v", err)
	}
	return true, "Left group successfully"
}

func getGroupInfo(client *whatsmeow.Client, groupJID string) (bool, string, *types.GroupInfo) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", nil
	}

	group, err := types.ParseJID(strings.TrimSpace(groupJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing group JID: %v", err), nil
	}
	if group.Server != types.GroupServer {
		return false, "group_jid must be a WhatsApp group JID ending in @g.us", nil
	}

	info, err := client.GetGroupInfo(context.Background(), group)
	if err != nil {
		return false, fmt.Sprintf("Failed to get group info: %v", err), nil
	}
	return true, "Group info retrieved successfully", info
}

func getGroupInviteLink(client *whatsmeow.Client, groupJID string, reset bool) (bool, string, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", ""
	}

	group, err := types.ParseJID(strings.TrimSpace(groupJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing group JID: %v", err), ""
	}
	if group.Server != types.GroupServer {
		return false, "group_jid must be a WhatsApp group JID ending in @g.us", ""
	}

	inviteLink, err := client.GetGroupInviteLink(context.Background(), group, reset)
	if err != nil {
		return false, fmt.Sprintf("Failed to get group invite link: %v", err), ""
	}
	return true, "Group invite link retrieved successfully", inviteLink
}

func findOwnParticipant(client *whatsmeow.Client, participants []types.GroupParticipant) *GroupParticipantResult {
	if client.Store == nil {
		return nil
	}

	var ids []types.JID
	if client.Store.ID != nil && !client.Store.ID.IsEmpty() {
		ids = append(ids, *client.Store.ID)
	}
	if !client.Store.LID.IsEmpty() {
		ids = append(ids, client.Store.LID)
	}

	for _, participant := range participants {
		for _, id := range ids {
			if participant.JID == id || participant.PhoneNumber == id || participant.LID == id {
				result := participantResult(participant)
				return &result
			}
		}
	}
	return nil
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string, *SentMessageInfo) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", nil
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), nil
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err), nil
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), nil
		}

		if debugContentLoggingEnabled() {
			fmt.Println("Media uploaded", resp)
		} else {
			fmt.Printf("Media uploaded: type=%v bytes=%d\n", mediaType, len(mediaData))
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err), nil
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	resp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), nil
	}

	timestamp := resp.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	sender := "me"
	if client.Store != nil && client.Store.ID != nil {
		sender = client.Store.ID.User
	}

	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg)
	return true, fmt.Sprintf("Message sent to %s", recipient), &SentMessageInfo{
		ID:            string(resp.ID),
		ChatJID:       recipientJID.String(),
		Sender:        sender,
		Content:       message,
		Timestamp:     timestamp,
		IsFromMe:      true,
		MediaType:     mediaType,
		Filename:      filename,
		URL:           url,
		MediaKey:      mediaKey,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		FileLength:    fileLength,
	}
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := sanitizeMediaFilename(doc.GetFileName(), "document")
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

func extractEventInfo(msg *waProto.Message) (EventInfo, bool) {
	if msg == nil {
		return EventInfo{}, false
	}

	event := msg.GetEventMessage()
	if event != nil {
		info := EventInfo{
			Name:               event.GetName(),
			Description:        event.GetDescription(),
			JoinLink:           event.GetJoinLink(),
			StartTime:          event.GetStartTime(),
			EndTime:            event.GetEndTime(),
			IsCanceled:         event.GetIsCanceled(),
			ExtraGuestsAllowed: event.GetExtraGuestsAllowed(),
			IsScheduleCall:     event.GetIsScheduleCall(),
			HasReminder:        event.GetHasReminder(),
			ReminderOffsetSec:  event.GetReminderOffsetSec(),
		}

		if location := event.GetLocation(); location != nil {
			info.LocationName = location.GetName()
			info.LocationAddress = location.GetAddress()
			if location.DegreesLatitude != nil {
				lat := location.GetDegreesLatitude()
				info.Latitude = &lat
			}
			if location.DegreesLongitude != nil {
				lon := location.GetDegreesLongitude()
				info.Longitude = &lon
			}
		}

		return info, true
	}

	invite := msg.GetEventInviteMessage()
	if invite == nil {
		return EventInfo{}, false
	}

	info := EventInfo{
		Name:        invite.GetEventTitle(),
		Description: invite.GetCaption(),
		StartTime:   invite.GetStartTime(),
		IsCanceled:  invite.GetIsCanceled(),
	}
	if info.Name == "" && invite.GetEventID() != "" {
		info.Name = invite.GetEventID()
	}
	if info.Description == "" && invite.GetEventID() != "" {
		info.Description = "event_id: " + invite.GetEventID()
	}

	return info, true
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	if event, ok := extractEventInfo(msg.Message); ok {
		err = messageStore.StoreEvent(
			msg.Info.ID,
			chatJID,
			sender,
			msg.Info.Timestamp,
			msg.Info.IsFromMe,
			event,
		)
		if err != nil {
			logger.Warnf("Failed to store event: %v", err)
		}
	}

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Avoid leaking private message contents into terminal logs by default.
		if debugContentLoggingEnabled() {
			if mediaType != "" {
				fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
			} else if content != "" {
				fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
			}
		} else if mediaType != "" {
			fmt.Printf("[%s] %s [media message: %s]\n", timestamp, direction, mediaType)
		} else {
			fmt.Printf("[%s] %s [text message]\n", timestamp, direction)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

type BackfillEventsRequest struct {
	ChatJID string `json:"chat_jid"`
	Count   int    `json:"count,omitempty"`
}

type BackfillEventsResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	safeChatDir := strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(chatJID)
	chatDir := filepath.Join(storeDir(), safeChatDir)
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0700); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	filename = sanitizeMediaFilename(filename, mediaType)
	localPath = filepath.Join(chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	if debugContentLoggingEnabled() {
		fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)
	} else {
		fmt.Println("Attempting to download media...")
	}

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	if debugContentLoggingEnabled() {
		fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	} else {
		fmt.Printf("Successfully downloaded %s media (%d bytes)\n", mediaType, len(mediaData))
	}
	return true, mediaType, filename, absPath, nil
}

func backfillEvents(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, count int) (bool, string, int) {
	if client == nil {
		return false, "Client is not initialized", 0
	}
	if !client.IsConnected() {
		return false, "Client is not connected to WhatsApp", 0
	}
	if client.Store == nil || client.Store.ID == nil {
		return false, "Client is not logged in", 0
	}
	if count <= 0 {
		count = 50
	}
	if count > 200 {
		count = 200
	}

	chat, err := types.ParseJID(strings.TrimSpace(chatJID))
	if err != nil {
		return false, fmt.Sprintf("Error parsing chat JID: %v", err), 0
	}

	anchor, err := messageStore.GetOldestMessageAnchor(chat.String())
	if err != nil {
		return false, fmt.Sprintf("Failed to find history anchor: %v", err), 0
	}
	if anchor == nil {
		return false, "No cached messages found for chat; open or sync the chat first so a history anchor exists", 0
	}

	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: anchor.IsFromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        anchor.ID,
		Timestamp: anchor.Timestamp,
	}

	historyMsg := client.BuildHistorySyncRequest(info, count)
	if historyMsg == nil {
		return false, "Failed to build history sync request", 0
	}

	if _, err := client.SendPeerMessage(context.Background(), historyMsg); err != nil {
		return false, fmt.Sprintf("Failed to request on-demand history sync: %v", err), 0
	}

	return true, "Requested on-demand history sync. New event cards will be stored if WhatsApp returns them in history sync.", count
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	// Remove query parameters
	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	// Create proper direct path format
	return "/" + pathPart
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, health *healthState, port int) error {
	token := bridgeToken()
	if token == "" {
		return fmt.Errorf("WHATSAPP_BRIDGE_TOKEN is required")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", healthHandler(health, token))

	// Handler for sending messages
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		if debugContentLoggingEnabled() {
			fmt.Println("Received request to send message", req.Message, req.MediaPath)
		} else {
			fmt.Printf("Received send request: has_message=%t has_media=%t\n", req.Message != "", req.MediaPath != "")
		}

		// Send the message
		success, message, sent := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)
		if debugContentLoggingEnabled() {
			fmt.Println("Message sent", success, message)
		} else {
			fmt.Println("Send request completed", success)
		}
		if success && sent != nil {
			err := messageStore.StoreMessage(
				sent.ID,
				sent.ChatJID,
				sent.Sender,
				sent.Content,
				sent.Timestamp,
				sent.IsFromMe,
				sent.MediaType,
				sent.Filename,
				sent.URL,
				sent.MediaKey,
				sent.FileSHA256,
				sent.FileEncSHA256,
				sent.FileLength,
			)
			if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
				_ = messageStore.StoreChat(sent.ChatJID, sent.ChatJID, sent.Timestamp)
				err = messageStore.StoreMessage(
					sent.ID,
					sent.ChatJID,
					sent.Sender,
					sent.Content,
					sent.Timestamp,
					sent.IsFromMe,
					sent.MediaType,
					sent.Filename,
					sent.URL,
					sent.MediaKey,
					sent.FileSHA256,
					sent.FileEncSHA256,
					sent.FileLength,
				)
			}
			if err != nil {
				fmt.Printf("Failed to store sent message: %v\n", err)
			}
		}
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	// Handler for downloading media
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	mux.HandleFunc("/api/events/backfill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req BackfillEventsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.ChatJID) == "" {
			http.Error(w, "Chat JID is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received event backfill request: chat=%s count=%d\n", req.ChatJID, req.Count)
		success, message, count := backfillEvents(client, messageStore, req.ChatJID, req.Count)
		fmt.Println("Event backfill request completed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(BackfillEventsResponse{
			Success: success,
			Message: message,
			Count:   count,
		})
	})

	mux.HandleFunc("/api/groups/participants/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req AddGroupParticipantsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}
		if len(req.Participants) == 0 {
			http.Error(w, "At least one participant is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received group participant add request: group=%s count=%d\n", req.GroupJID, len(req.Participants))
		success, message, participants := addGroupParticipants(client, req.GroupJID, req.Participants)
		fmt.Println("Group participant add request completed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(AddGroupParticipantsResponse{
			Success:      success,
			Message:      message,
			Participants: participants,
		})
	})

	mux.HandleFunc("/api/groups/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req CreateGroupRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(req.Name) == "" {
			http.Error(w, "Group name is required", http.StatusBadRequest)
			return
		}
		if len(req.Participants) == 0 {
			http.Error(w, "At least one participant is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received group create request: name=%q count=%d\n", req.Name, len(req.Participants))
		success, message, groupJID, participants := createGroup(client, req.Name, req.Participants)
		fmt.Println("Group create request completed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(CreateGroupResponse{
			Success:      success,
			Message:      message,
			GroupJID:     groupJID,
			Participants: participants,
		})
	})

	mux.HandleFunc("/api/groups/participants/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req RemoveGroupParticipantsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}
		if len(req.Participants) == 0 {
			http.Error(w, "At least one participant is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received group participant remove request: group=%s count=%d\n", req.GroupJID, len(req.Participants))
		success, message, participants := removeGroupParticipants(client, req.GroupJID, req.Participants)
		fmt.Println("Group participant remove request completed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(RemoveGroupParticipantsResponse{
			Success:      success,
			Message:      message,
			Participants: participants,
		})
	})

	mux.HandleFunc("/api/groups/leave", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req LeaveGroupRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received group leave request: group=%s\n", req.GroupJID)
		success, message := leaveGroup(client, req.GroupJID)
		fmt.Println("Group leave request completed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(LeaveGroupResponse{
			Success: success,
			Message: message,
		})
	})

	mux.HandleFunc("/api/groups/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req GroupInfoRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}

		success, message, info := getGroupInfo(client, req.GroupJID)
		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GroupInfoResponse{
				Success: success,
				Message: message,
			})
			return
		}

		participants := make([]GroupParticipantResult, len(info.Participants))
		for i, participant := range info.Participants {
			participants[i] = participantResult(participant)
		}

		json.NewEncoder(w).Encode(GroupInfoResponse{
			Success:          success,
			Message:          message,
			GroupJID:         info.JID.String(),
			Name:             info.Name,
			MemberAddMode:    string(info.MemberAddMode),
			ParticipantCount: info.ParticipantCount,
			OwnParticipant:   findOwnParticipant(client, info.Participants),
			Participants:     participants,
		})
	})

	mux.HandleFunc("/api/groups/invite-link", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}

		var req GroupInviteLinkRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}

		success, message, inviteLink := getGroupInviteLink(client, req.GroupJID, req.Reset)
		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(GroupInviteLinkResponse{
			Success:    success,
			Message:    message,
			InviteLink: inviteLink,
		})
	})

	// Start the server
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", serverAddr, err)
	}

	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.Serve(listener, mux); err != nil && err != http.ErrServerClosed {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()

	return nil
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	dir := storeDir()
	// Create directory for database if it doesn't exist (owner-only).
	if err := os.MkdirAll(dir, 0700); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	whatsappDSN := fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(dir, "whatsapp.db"))
	container, err := sqlstore.New(context.Background(), "sqlite3", whatsappDSN, dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Track the real connection state for /api/health, independent of the
	// message-ingest handler below.
	health := newHealthState()
	client.AddEventHandler(health.HandleEvent)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Warnf("Recovered from WhatsApp event handler panic: %v\n%s", recovered, debug.Stack())
			}
		}()

		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out by WhatsApp (on_connect=%t, reason=%s). Please scan QR code to log in again", v.OnConnect, v.Reason.String())
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		if os.Getenv("WHATSAPP_BRIDGE_SUPPRESS_QR") == "1" {
			logger.Errorf("WhatsApp is not paired. Run scripts/start-bridge.sh in a visible terminal to scan the QR code.")
			return
		}

		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	if err := startRESTServer(client, messageStore, health, bridgePort()); err != nil {
		logger.Errorf("Failed to start REST API server: %v", err)
		return
	}

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		if debugContentLoggingEnabled() {
			logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		} else {
			logger.Infof("Using existing chat metadata")
		}
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		if debugContentLoggingEnabled() {
			logger.Infof("Getting name for group: %s", chatJID)
		} else {
			logger.Infof("Getting group chat name")
		}

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		if debugContentLoggingEnabled() {
			logger.Infof("Using group name: %s", name)
		} else {
			logger.Infof("Using group chat name")
		}
	} else {
		// This is an individual contact
		if debugContentLoggingEnabled() {
			logger.Infof("Getting name for contact: %s", chatJID)
		} else {
			logger.Infof("Getting contact name")
		}

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		if debugContentLoggingEnabled() {
			logger.Infof("Using contact name: %s", name)
		} else {
			logger.Infof("Using contact name")
		}
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			if debugContentLoggingEnabled() {
				logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			} else {
				logger.Warnf("Failed to parse history-sync JID: %v", err)
			}
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		logger.Infof("History sync conversation metadata: chat=%s messages=%d", chatJID, len(messages))
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				event, hasEvent := extractEventInfo(msg.Message.Message)

				messageShape := describeMessageShape(msg.Message.Message)
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}
				if debugContentLoggingEnabled() {
					logger.Infof("Message content: id=%s content=%v, Media Type: %v, Has Event: %t, shape=%s", msgID, content, mediaType, hasEvent, messageShape)
				} else {
					logger.Infof("History message metadata: id=%s has_content=%t, media_type=%s, has_event=%t, shape=%s", msgID, content != "", mediaType, hasEvent, messageShape)
				}

				// Skip messages with no content, media, or event card.
				if content == "" && mediaType == "" && !hasEvent {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						if client.Store != nil && client.Store.ID != nil {
							sender = client.Store.ID.User
						} else {
							sender = "me"
						}
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				if hasEvent {
					err = messageStore.StoreEvent(msgID, chatJID, sender, timestamp, isFromMe, event)
					if err != nil {
						logger.Warnf("Failed to store history event: %v", err)
					} else {
						syncedCount++
					}
				}

				if content != "" || mediaType != "" {
					err = messageStore.StoreMessage(
						msgID,
						chatJID,
						sender,
						content,
						timestamp,
						isFromMe,
						mediaType,
						filename,
						url,
						mediaKey,
						fileSHA256,
						fileEncSHA256,
						fileLength,
					)
					if err != nil {
						logger.Warnf("Failed to store history message: %v", err)
					} else {
						syncedCount++
						if debugContentLoggingEnabled() {
							if mediaType != "" {
								logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
									timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
							} else {
								logger.Infof("Stored message: [%s] %s -> %s: %s",
									timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
							}
						} else {
							logger.Infof("Stored message metadata: [%s] has_content=%t, media_type=%s",
								timestamp.Format("2006-01-02 15:04:05"), content != "", mediaType)
						}
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
