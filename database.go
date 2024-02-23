package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// constants
const (
	DefaultMaxNumTries = 10
)

// Prompt struct
type Prompt struct {
	gorm.Model

	ChatID   int64 `gorm:"index"`
	UserID   int64
	Username string

	Text   string
	Tokens int `gorm:"index"`

	Result ParsedItem
}

// Log struct is for logging messages
type Log struct {
	gorm.Model

	Type    string
	Message string
}

// ParsedItem struct
type ParsedItem struct {
	gorm.Model

	Successful bool `gorm:"index"`
	Tokens     int  `gorm:"index"`

	PromptID int64 // foreign key
}

// QueueItem is a struct for queue items
type QueueItem struct {
	gorm.Model

	ID          int64
	ChatID      int64 `gorm:"index:idx_queue1;index:idx_queue4"`
	MessageID   int64
	Message     string
	EnqueuedOn  time.Time  `gorm:"index:idx_queue2;index:idx_queue3;index:idx_queue4;index:idx_queue5"`
	FireOn      time.Time  `gorm:"index:idx_queue5"`
	DeliveredOn *time.Time `gorm:"index:idx_queue1;index:idx_queue2;index:idx_queue3;index:idx_queue4;index:idx_queue5"`
	NumTries    int        `gorm:"index:idx_queue3;index:idx_queue5"`
}

// TemporaryMessage is a struct for temporary message for handling inline queries
type TemporaryMessage struct {
	gorm.Model

	ID        int64
	ChatID    int64 `gorm:"index:idx_temp_messages1"`
	MessageID int64 `gorm:"index:idx_temp_messages1"`
	Message   string
	SavedOn   time.Time
}

// Database struct
type Database struct {
	db *gorm.DB
}

// OpenDatabase opens and returns a database at given path: `dbPath`.
func OpenDatabase(dbPath string) (database *Database, err error) {
	var db *gorm.DB
	db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		PrepareStmt: true,
	})

	if err == nil {
		// migrate tables
		if err := db.AutoMigrate(
			&Prompt{},
			&Log{},
			&ParsedItem{},
			&QueueItem{},
			&TemporaryMessage{},
		); err != nil {
			log.Printf("failed to migrate databases: %s", err)
		}

		return &Database{db: db}, nil
	}

	return nil, err
}

// SavePrompt saves `prompt`.
func (d *Database) SavePrompt(prompt Prompt) (err error) {
	tx := d.db.Save(&prompt)
	return tx.Error
}

// save log for given type and message
func (d *Database) saveLog(typ, msg string) (err error) {
	tx := d.db.Create(&Log{
		Type:    typ,
		Message: msg,
	})

	return tx.Error
}

// Log logs a message
func (d *Database) Log(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)

	if err := d.saveLog("log", msg); err != nil {
		log.Printf("failed to save log message: %s", err)
	}
}

// LogError logs an error message
func (d *Database) LogError(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)

	if err := d.saveLog("err", msg); err != nil {
		log.Printf("failed to save error message: %s", err)
	}
}

// GetLogs fetches `latestN` number of latest logs
func (d *Database) GetLogs(latestN int) (logs []Log, err error) {
	tx := d.db.Order("id desc").Limit(latestN).Find(&logs)

	return logs, tx.Error
}

// SaveTemporaryMessage saves a temporary message
func (d *Database) SaveTemporaryMessage(chatID int64, messageID int64, message string) (result bool, err error) {
	res := d.db.Create(&TemporaryMessage{
		ChatID:    chatID,
		MessageID: messageID,
		Message:   message,
		SavedOn:   time.Now(),
	})

	return res.RowsAffected > 0, res.Error
}

// LoadTemporaryMessage retrieves a temporary message
func (d *Database) LoadTemporaryMessage(chatID, messageID int64) (result TemporaryMessage, err error) {
	res := d.db.Where("chat_id = ? and message_id = ?", chatID, messageID).First(&result)

	return result, res.Error
}

// DeleteTemporaryMessage deletes given temporary message
func (d *Database) DeleteTemporaryMessage(chatID int64, messageID int64) (result bool, err error) {
	res := d.db.Where("chat_id = ? and message_id = ?", chatID, messageID).Delete(&TemporaryMessage{ChatID: chatID, MessageID: messageID})

	return res.RowsAffected > 0, res.Error
}

// Enqueue enques given message
func (d *Database) Enqueue(chatID int64, messageID int64, message string, fireOn time.Time) (result bool, err error) {
	res := d.db.Save(&QueueItem{
		ChatID:    chatID,
		MessageID: messageID,
		Message:   message,
		FireOn:    fireOn,
	})

	return res.RowsAffected > 0, res.Error
}

// DeliverableQueueItems fetches all items from the queue which need to be delivered right now.
func (d *Database) DeliverableQueueItems(maxNumTries int) (result []QueueItem, err error) {
	if maxNumTries <= 0 {
		maxNumTries = DefaultMaxNumTries
	}

	res := d.db.Order("enqueued_on desc").Where("delivered_on is null and num_tries < ? and fire_on <= ?", maxNumTries, time.Now()).Find(&result)

	return result, res.Error
}

// UndeliveredQueueItems fetches all undelivered items from the queue.
func (d *Database) UndeliveredQueueItems(chatID int64) (result []QueueItem, err error) {
	res := d.db.Order("fire_on asc").Where("chat_id = ? and delivered_on is null", chatID).Find(&result)

	return result, res.Error
}

// GetQueueItem fetches a queue item
func (d *Database) GetQueueItem(chatID, queueID int64) (result QueueItem, err error) {
	res := d.db.Where("id = ? and chat_id = ?", queueID, chatID).First(&result)

	return result, res.Error
}

// DeleteQueueItem deletes a queue item
func (d *Database) DeleteQueueItem(chatID, queueID int64) (result bool, err error) {
	res := d.db.Where("id = ? and chat_id = ?", queueID, chatID).Delete(&QueueItem{})

	return res.RowsAffected > 0, res.Error
}

// IncreaseNumTries increases the number of tries of a queue item
func (d *Database) IncreaseNumTries(chatID, queueID int64) (result bool, err error) {
	res := d.db.Model(&QueueItem{}).Where("id = ? and chat_id = ?", queueID, chatID).Update("num_tries", gorm.Expr("num_tries + 1"))

	return res.RowsAffected > 0, res.Error
}

// MarkQueueItemAsDelivered makes a queue item as delivered
func (d *Database) MarkQueueItemAsDelivered(chatID, queueID int64) (result bool, err error) {
	res := d.db.Model(&QueueItem{}).Where("id = ? and chat_id = ?", queueID, chatID).Update("delivered_on", time.Now())

	return res.RowsAffected > 0, res.Error
}

// Stats retrieves stats from database as a string.
func (d *Database) Stats() string {
	lines := []string{}

	var prompt Prompt
	if tx := d.db.First(&prompt); tx.Error == nil {
		lines = append(lines, fmt.Sprintf("Since <i>%s</i>", prompt.CreatedAt.Format("2006-01-02 15:04:05")))
		lines = append(lines, "")
	}

	var count int64
	if tx := d.db.Table("prompts").Select("count(distinct chat_id) as count").Scan(&count); tx.Error == nil {
		lines = append(lines, fmt.Sprintf("* Chats: <b>%d</b>", count))
	}

	var sumAndCount struct {
		Sum   int64
		Count int64
	}
	if tx := d.db.Table("prompts").Select("sum(tokens) as sum, count(id) as count").Where("tokens > 0").Scan(&sumAndCount); tx.Error == nil {
		lines = append(lines, fmt.Sprintf("* Prompts: <b>%d</b> (Total tokens: <b>%d</b>)", sumAndCount.Count, sumAndCount.Sum))
	}
	if tx := d.db.Table("parsed_items").Select("sum(tokens) as sum, count(id) as count").Where("successful = 1").Scan(&sumAndCount); tx.Error == nil {
		lines = append(lines, fmt.Sprintf("* Completions: <b>%d</b> (Total tokens: <b>%d</b>)", sumAndCount.Count, sumAndCount.Sum))
	}
	if tx := d.db.Table("parsed_items").Select("count(id) as count").Where("successful = 0").Scan(&count); tx.Error == nil {
		lines = append(lines, fmt.Sprintf("* Errors: <b>%d</b>", count))
	}

	if len(lines) > 0 {
		return strings.Join(lines, "\n")
	}

	return msgDatabaseEmpty
}
