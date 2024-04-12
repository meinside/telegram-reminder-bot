package main

// bot.go

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/meinside/infisical-go"
	"github.com/meinside/infisical-go/helper"
	tg "github.com/meinside/telegram-bot-go"
	"github.com/meinside/version-go"

	"github.com/google/generative-ai-go/genai"
	"github.com/tailscale/hujson"
	"google.golang.org/api/option"
)

const (
	intervalSeconds = 1

	cmdStart         = "/start" // (internal)
	cmdStats         = "/stats"
	cmdHelp          = "/help"
	cmdCancel        = "/cancel"
	cmdLoad          = "/load" // (internal)
	cmdListReminders = "/list"

	msgStart                 = "This bot will reserve your messages and notify you at desired times, with ChatGPT API :-)"
	msgCmdNotSupported       = "Not a supported bot command: %s"
	msgTypeNotSupported      = "Not a supported message type."
	msgDatabaseNotConfigured = "Database not configured. Set `db_filepath` in your config file."
	msgDatabaseEmpty         = "Database is empty."
	msgTokenCount            = "<b>%d</b> tokens in <b>%d</b> chars <i>(cl100k_base)</i>"
	msgHelp                  = `Help message here:

/list : list all the active reminders.
/cancel : cancel a reminder.
/stats : show stats of this bot.
/help : show this help message.

<i>version: %s</i>
`
	msgCommandCanceled        = "Command was canceled."
	msgReminderCanceledFormat = "Reminder was canceled: %s"
	msgError                  = "An error has occurred."
	msgResponseFormat         = `Will notify "%s" on %s.`
	msgSaveFailedFormat       = "Failed to save reminder: %s (%s)"
	msgSelectWhat             = "Which time do you want to select for message: \"%s\"?"
	msgCancelWhat             = "Which one do you want to cancel?"
	msgCancel                 = "Cancel"
	msgParseFailedFormat      = "Failed to understand message: %s"
	msgListItemFormat         = "☑ %s; %s"
	msgNoReminders            = "There is no registered reminder."
	msgNoClue                 = "There was no clue for the time in the message."

	datetimeFormat = "2006.01.02 15:04" // yyyy.mm.dd hh:MM

	// default configs
	defaultMonitorIntervalSeconds  = 30
	defaultTelegramIntervalSeconds = 60
	defaultMaxNumTries             = 5
	defaultGenerativeModel         = "gemini-pro"
)

var _location *time.Location

// config struct for loading a configuration file
type config struct {
	GoogleGenerativeModel string `json:"google_generative_model,omitempty"`

	MonitorIntervalSeconds  int    `json:"monitor_interval_seconds"`
	TelegramIntervalSeconds int    `json:"telegram_interval_seconds"`
	MaxNumTries             int    `json:"max_num_tries"`
	DBFilepath              string `json:"db_filepath"`

	// other optional configurations
	AllowedTelegramUsers []string `json:"allowed_telegram_users"`
	DefaultHour          int      `json:"default_hour,omitempty"`
	Verbose              bool     `json:"verbose,omitempty"`

	// token and api key
	TelegramBotToken string `json:"telegram_bot_token"`
	GoogleAIAPIKey   string `json:"google_ai_api_key"`

	// or Infisical settings
	Infisical *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`

		WorkspaceID string               `json:"workspace_id"`
		Environment string               `json:"environment"`
		SecretType  infisical.SecretType `json:"secret_type"`

		TelegramBotTokenKeyPath string `json:"telegram_bot_token_key_path"`
		GoogleAIAPIKeyKeyPath   string `json:"google_ai_api_key_key_path"`
	} `json:"infisical,omitempty"`
}

// load config at given path
func loadConfig(fpath string) (conf config, err error) {
	var bytes []byte
	if bytes, err = os.ReadFile(fpath); err == nil {
		if bytes, err = standardizeJSON(bytes); err == nil {
			if err = json.Unmarshal(bytes, &conf); err == nil {
				if (conf.TelegramBotToken == "" || conf.GoogleAIAPIKey == "") && conf.Infisical != nil {
					// read token and api key from infisical
					var botToken, apiKey string

					var kvs map[string]string
					kvs, err = helper.Values(
						conf.Infisical.ClientID,
						conf.Infisical.ClientSecret,
						conf.Infisical.WorkspaceID,
						conf.Infisical.Environment,
						conf.Infisical.SecretType,
						[]string{
							conf.Infisical.TelegramBotTokenKeyPath,
							conf.Infisical.GoogleAIAPIKeyKeyPath,
						},
					)

					var exists bool
					if botToken, exists = kvs[conf.Infisical.TelegramBotTokenKeyPath]; exists {
						conf.TelegramBotToken = botToken
					}
					if apiKey, exists = kvs[conf.Infisical.GoogleAIAPIKeyKeyPath]; exists {
						conf.GoogleAIAPIKey = apiKey
					}
				}

				// set default/fallback values
				if conf.MonitorIntervalSeconds <= 0 {
					conf.MonitorIntervalSeconds = defaultMonitorIntervalSeconds
				}
				if conf.TelegramIntervalSeconds <= 0 {
					conf.TelegramIntervalSeconds = defaultTelegramIntervalSeconds
				}
				if conf.MaxNumTries <= 0 {
					conf.MaxNumTries = defaultMaxNumTries
				}
				if conf.GoogleGenerativeModel == "" {
					conf.GoogleGenerativeModel = defaultGenerativeModel
				}
				if conf.DefaultHour < 0 || conf.DefaultHour >= 24 {
					conf.DefaultHour = 0
				}
			}
		}
	}

	return conf, err
}

// standardize given JSON (JWCC) bytes
func standardizeJSON(b []byte) ([]byte, error) {
	ast, err := hujson.Parse(b)
	if err != nil {
		return b, err
	}
	ast.Standardize()

	return ast.Pack(), nil
}

// remove markdown from text
func removeMarkdown(md string) (result string) {
	lines := []string{}

	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(line, "```") {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// launch bot with given parameters
func runBot(conf config) {
	var err error

	_location, _ = time.LoadLocation("Local")

	token := conf.TelegramBotToken
	apiKey := conf.GoogleAIAPIKey

	bot := tg.NewClient(token)

	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		logErrorAndDie(nil, "failed to create API client: %s", err)
	}
	defer client.Close()

	// open database
	var db *Database
	if db, err = OpenDatabase(conf.DBFilepath); err != nil {
		logErrorAndDie(nil, "failed to open database: %s", err)
	}

	_ = bot.DeleteWebhook(false) // delete webhook before polling updates
	if b := bot.GetMe(); b.Ok {
		logInfo("launching bot: %s", userName(b.Result))

		// monitor queue
		logInfo("starting monitoring queue...")
		go monitorQueue(
			time.NewTicker(time.Duration(conf.MonitorIntervalSeconds)*time.Second),
			bot,
			conf,
			db,
		)

		// set message handler
		bot.SetMessageHandler(func(b *tg.Bot, update tg.Update, message tg.Message, edited bool) {
			if !isAllowed(conf, update) {
				logDebug(conf, "message not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleMessage(ctx, b, client, conf, db, update, message)
		})

		// set callback query handler
		bot.SetCallbackQueryHandler(func(b *tg.Bot, update tg.Update, callbackQuery tg.CallbackQuery) {
			if !isAllowed(conf, update) {
				logDebug(conf, "callback query not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleCallbackQuery(b, db, callbackQuery)
		})

		// set command handlers
		bot.AddCommandHandler(cmdStart, startCommandHandler(conf, db))
		bot.AddCommandHandler(cmdListReminders, listRemindersCommandHandler(conf, db))
		bot.AddCommandHandler(cmdStats, statsCommandHandler(conf, db))
		bot.AddCommandHandler(cmdHelp, helpCommandHandler(conf, db))
		bot.AddCommandHandler(cmdCancel, cancelCommandHandler(conf, db))
		bot.SetNoMatchingCommandHandler(noSuchCommandHandler(conf, db))

		// poll updates
		bot.StartPollingUpdates(0, intervalSeconds, func(b *tg.Bot, update tg.Update, err error) {
			if err == nil {
				if !isAllowed(conf, update) {
					logDebug(conf, "not allowed: %s", userNameFromUpdate(update))
					return
				}

				// type not supported
				if message := messageFromUpdate(update); message != nil {
					send(b, conf, db, msgTypeNotSupported, message.Chat.ID, &message.MessageID)
				}
			} else {
				logError(db, "failed to fetch updates: %s", err)
			}
		})
	} else {
		logInfo("failed to get bot info: %s", *b.Description)
	}
}

// checks if given update is allowed or not
func isAllowed(conf config, update tg.Update) bool {
	var username string
	if update.HasMessage() && update.Message.From.Username != nil {
		username = *update.Message.From.Username
	} else if update.HasEditedMessage() && update.EditedMessage.From.Username != nil {
		username = *update.EditedMessage.From.Username
	} else if update.HasCallbackQuery() && update.CallbackQuery.From.Username != nil {
		username = *update.CallbackQuery.From.Username
	}

	for _, allowedUser := range conf.AllowedTelegramUsers {
		if allowedUser == username {
			return true
		}
	}

	return false
}

// poll queue items periodically
func monitorQueue(monitor *time.Ticker, client *tg.Bot, conf config, db *Database) {
	for range monitor.C {
		processQueue(client, conf, db)
	}
}

// process queue item
func processQueue(client *tg.Bot, conf config, db *Database) {
	if queue, err := db.DeliverableQueueItems(conf.MaxNumTries); err == nil {
		logDebug(conf, "checking queue: %d items...", len(queue))

		for _, q := range queue {
			go func(q QueueItem) {
				message := q.Message

				// send it
				sent := client.SendMessage(
					q.ChatID,
					message,
					tg.OptionsSendMessage{}.
						SetReplyMarkup(defaultReplyMarkup()).
						SetReplyParameters(tg.NewReplyParameters(q.MessageID)))

				if sent.Ok {
					// mark as delivered
					if _, err := db.MarkQueueItemAsDelivered(q.ChatID, q.ID); err != nil {
						logError(db, "failed to mark chat id: %d, queue id: %d (%s)", q.ChatID, q.ID, err)
					}
				} else {
					logError(db, "failed to send reminder: %s", *sent.Description)
				}

				// increase num tries
				if _, err := db.IncreaseNumTries(q.ChatID, q.ID); err != nil {
					logError(db, "failed to increase num tries for chat id: %d, queue id: %d (%s)", q.ChatID, q.ID, err)
				}
			}(q)
		}
	} else {
		logError(db, "failed to process queue: %s", err)
	}
}

// handle allowed message update from telegram bot api
func handleMessage(ctx context.Context, bot *tg.Bot, client *genai.Client, conf config, db *Database, update tg.Update, message tg.Message) {
	var msg string

	chatID := message.Chat.ID

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup())

	// 'is typing...'
	bot.SendChatAction(chatID, tg.ChatActionTyping, tg.OptionsSendChatAction{})

	if message := messageFromUpdate(update); message != nil {
		if message.HasText() {
			txt := *message.Text
			if parsed, errs := parse(ctx, client, conf, db, *message, txt); len(errs) == 0 {
				parsed = filterParsed(conf, parsed)

				if len(parsed) == 1 {
					what := parsed[0].Message
					when := parsed[0].When

					if _, err := db.Enqueue(chatID, message.MessageID, what, when); err == nil {
						msg = fmt.Sprintf(msgResponseFormat,
							what,
							datetimeToStr(when),
						)
					} else {
						msg = fmt.Sprintf(msgSaveFailedFormat, what, err)
					}
				} else if len(parsed) > 0 {
					if _, err := db.SaveTemporaryMessage(chatID, message.MessageID, parsed[0].Message); err == nil {
						msg = fmt.Sprintf(msgSelectWhat, parsed[0].Message)

						// options for inline keyboards
						options.SetReplyMarkup(tg.NewInlineKeyboardMarkup(
							datetimeButtonsForCallbackQuery(parsed, chatID, message.MessageID),
						))
					} else {
						msg = msgError
					}
				} else {
					msg = msgNoClue
				}
			} else {
				msg = fmt.Sprintf(msgParseFailedFormat, errors.Join(errs...))
			}
		} else {
			logInfo("no text in usable message from update.")

			msg = msgTypeNotSupported
		}
	} else {
		logInfo("no usable message from update.")

		msg = msgTypeNotSupported
	}

	// fallback message
	if len(msg) <= 0 {
		msg = msgError
	}

	// send message
	if sent := bot.SendMessage(chatID, msg, options); !sent.Ok {
		logError(db, "failed to send message: %s", *sent.Description)
	}
}

// handle allowed callback query from telegram bot api
func handleCallbackQuery(b *tg.Bot, db *Database, query tg.CallbackQuery) {
	data := *query.Data

	msg := msgError

	if strings.HasPrefix(data, cmdCancel) {
		if data == cmdCancel {
			msg = msgCommandCanceled
		} else {
			cancelParam := strings.TrimSpace(strings.Replace(data, cmdCancel, "", 1))
			if queueID, err := strconv.Atoi(cancelParam); err == nil {
				if item, err := db.GetQueueItem(query.Message.Chat.ID, int64(queueID)); err == nil {
					if _, err := db.DeleteQueueItem(query.Message.Chat.ID, int64(queueID)); err == nil {
						msg = fmt.Sprintf(msgReminderCanceledFormat, item.Message)
					} else {
						logError(db, "failed to delete reminder: %s", err)
					}
				} else {
					logError(db, "failed to get reminder: %s", err)
				}
			} else {
				logError(db, "unprocessable callback query: %s", data)
			}
		}
	} else if strings.HasPrefix(data, cmdLoad) {
		params := strings.Split(strings.TrimSpace(strings.Replace(data, cmdLoad, "", 1)), "/")

		if len(params) >= 3 {
			if chatID, err := strconv.ParseInt(params[0], 10, 64); err == nil {
				if messageID, err := strconv.ParseInt(params[1], 10, 64); err == nil {
					if saved, err := db.LoadTemporaryMessage(chatID, messageID); err == nil {
						if when, err := time.ParseInLocation(datetimeFormat, params[2], _location); err == nil {
							if _, err := db.Enqueue(chatID, messageID, saved.Message, when); err == nil {
								msg = fmt.Sprintf(msgResponseFormat,
									saved.Message,
									datetimeToStr(when),
								)

								// delete temporary message
								if _, err := db.DeleteTemporaryMessage(chatID, messageID); err != nil {
									logError(db, "failed to delete temporary message: %s", err)
								}
							} else {
								msg = fmt.Sprintf(msgSaveFailedFormat, saved.Message, err)
							}
						} else {
							logError(db, "failed to parse time: %s", err)
						}
					} else {
						logError(db, "failed to load temporary message with chat id: %d, message id: %d", chatID, messageID)
					}
				} else {
					logError(db, "failed to convert message id: %s", err)
				}
			} else {
				logError(db, "failed to convert chat id: %s", err)
			}
		} else {
			logError(db, "malformed inline keyboard data: %s", data)
		}
	} else {
		logError(db, "unprocessable callback query: %s", data)
	}

	// answer callback query
	if apiResult := b.AnswerCallbackQuery(query.ID, map[string]interface{}{"text": msg}); apiResult.Ok {
		// edit message and remove inline keyboards
		options := map[string]interface{}{
			"chat_id":    query.Message.Chat.ID,
			"message_id": query.Message.MessageID,
		}
		if apiResult := b.EditMessageText(msg, options); !apiResult.Ok {
			logError(db, "failed to edit message text: %s", *apiResult.Description)
		}
	} else {
		logError(db, "failed to answer callback query: %+v", query)
	}
}

// get usable message from given update
func messageFromUpdate(update tg.Update) (message *tg.Message) {
	if update.HasMessage() && update.Message.HasText() {
		message = update.Message
	} else if update.HasMessage() && update.Message.HasDocument() {
		message = update.Message
	} else if update.HasEditedMessage() && update.EditedMessage.HasText() {
		message = update.EditedMessage
	}

	if message == nil {
		logInfo("no usable message from update.")
	}

	return message
}

// send given message to the chat
func send(bot *tg.Bot, conf config, db *Database, message string, chatID int64, messageID *int64) {
	_ = bot.SendChatAction(chatID, tg.ChatActionTyping, nil)

	logDebug(conf, "[verbose] sending message to chat(%d): '%s'", chatID, message)

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup()).
		SetParseMode(tg.ParseModeHTML)
	if messageID != nil {
		options.SetReplyParameters(tg.NewReplyParameters(*messageID))
	}
	if res := bot.SendMessage(chatID, message, options); !res.Ok {
		logError(db, "failed to send message: %s", *res.Description)
	}
}

// type for parsed items
type parsedItem struct {
	Message   string
	When      time.Time
	Generated bool
}

// response JSON type
type responseJSON struct {
	Text     string  `json:"text"`
	Datetime *string `json:"datetime,omitempty"`
}

// function declarations for genai model
func fnDeclarations_reserveMessage(conf config) []*genai.FunctionDeclaration {
	return []*genai.FunctionDeclaration{
		{
			Name:        "reserveMessage",
			Description: "This function reserves a text message and sends it back at the desired time.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"text": {
						Type:        genai.TypeString,
						Description: "A message text to reserve.",
						Nullable:    false,
					},
					"datetime": {
						Type:        genai.TypeString,
						Description: fmt.Sprintf("A datetime string in \"yyyy.mm.dd hh:MM\" format, when the 'text' message should be sent back. If the date is unclear, assume it to be null. If the time is unclear, assume it to be \"%02d:00\".", conf.DefaultHour),
						Nullable:    true,
					},
				},
			},
		},
	}
}

// handle function call
func handleFnCall(fn genai.FunctionCall) (result parsedItem, err error) {
	if fn.Name == "reserveMessage" {
		var text string
		if txt, exists := fn.Args["text"]; exists {
			if txt, ok := txt.(string); ok {
				text = txt
			}
		}
		if dt, exists := fn.Args["datetime"]; exists {
			if dt, ok := dt.(string); ok {
				if t, err := time.ParseInLocation(datetimeFormat, dt, _location); err == nil {
					result = parsedItem{
						Message:   text,
						When:      t,
						Generated: false,
					}
				} else {
					err = fmt.Errorf("failed to parse 'datetime': %s", err)
				}
			} else {
				err = fmt.Errorf("malformed `datetime` in function call: %+v", dt)
			}
		} else {
			err = fmt.Errorf("no `datetime` in function call")
		}
	} else {
		err = fmt.Errorf("no function declaration for name: %s", fn.Name)
	}

	return result, err
}

// parse given string, generate items from the parsed ones, and return them
func parse(ctx context.Context, client *genai.Client, conf config, db *Database, message tg.Message, text string) (result []parsedItem, errs []error) {
	result = []parsedItem{}
	errs = []error{}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := userName(message.From)

	// model for generation
	model := client.GenerativeModel(conf.GoogleGenerativeModel)
	model.SafetySettings = safetySettings(genai.HarmBlockOnlyHigh) // set safety settings
	model.Tools = []*genai.Tool{                                   // set function declarations
		{
			FunctionDeclarations: fnDeclarations_reserveMessage(conf),
		},
	}

	now := datetimeToStr(time.Now())

	// generate text
	if generated, err := model.GenerateContent(
		ctx,
		genai.Text(fmt.Sprintf(
			`Current time is %s. Process following text: %s.`,
			now,
			text,
		)),
	); err != nil {
		errs = append(errs, fmt.Errorf("failed to generate text: %s", err))

		// log failure
		var numTokens int32
		if counted, err := model.CountTokens(ctx, genai.Text(text)); err == nil {
			numTokens = counted.TotalTokens
		}
		savePromptAndResult(db, chatID, userID, username, text, int(numTokens), 0, false)

		logError(db, "failed to generate text: %s", err)
	} else {
		if len(generated.Candidates) <= 0 {
			logError(db, "there was no returned candidate")
		} else {
			for _, candidate := range generated.Candidates {
				content := candidate.Content

				if len(content.Parts) > 0 {
					part := content.Parts[0]

					if fnCall, ok := part.(genai.FunctionCall); ok { // if it is a function call,
						if handled, err := handleFnCall(fnCall); err == nil {
							// append result
							result = append(result, handled)
						} else {
							errs = append(errs, err)

							logError(db, fmt.Sprintf("failed to handle function call: %s", err))
						}
					} else if text, ok := part.(genai.Text); ok { // if it is a text,
						errs = append(errs, fmt.Errorf("%s", text))

						logError(db, "non-function text was returned: %+v", part)
					} else { // otherwise,
						errs = append(errs, fmt.Errorf("no usable data in the part"))

						logError(db, "there was no usable data in the returned part: %+v", part)
					}
				} else {
					errs = append(errs, fmt.Errorf("no part in content"))

					logError(db, "there was no part in the returned content")
				}
			}
		}
	}

	// log success
	if len(errs) <= 0 {
		var numTokens int32
		if counted, err := model.CountTokens(ctx, genai.Text(text)); err == nil {
			numTokens = counted.TotalTokens
		}
		savePromptAndResult(db, chatID, userID, username, text, int(numTokens), 0, true)
	}

	return result, errs
}

// filter parsed items to be all valid
func filterParsed(conf config, parsed []parsedItem) (filtered []parsedItem) {
	// add some generated items for convenience
	generated := []parsedItem{}
	for _, p := range parsed {
		// save it as it is,
		generated = append(generated, p)

		// and add generated ones,
		when := p.When.In(_location)
		hour, minute := when.Hour(), when.Minute()
		if hour == 0 && minute == 0 {
			// default hour
			generated = append(generated, parsedItem{
				Message:   p.Message,
				When:      p.When.In(_location).Add(time.Hour * time.Duration(conf.DefaultHour)),
				Generated: true,
			})
		} else if hour < 12 {
			// add 12 hours if it is AM
			generated = append(generated, parsedItem{
				Message:   p.Message,
				When:      p.When.In(_location).Add(time.Hour * 12),
				Generated: true,
			})
		}
	}

	// remove already-passed or duplicated ones
	filtered = []parsedItem{}
	duplicated := map[string]bool{}
	now := time.Now()
	for _, p := range generated {
		when := p.When.In(_location)

		// remove duplicated ones,
		dup := when.Format(datetimeFormat)
		if _, exists := duplicated[dup]; exists {
			continue
		} else {
			duplicated[dup] = true // mark as duplicated,

			// and remove already-passed ones
			if when.After(now) {
				filtered = append(filtered, p)
			}
		}
	}

	return filtered
}

// generate safety settings for all supported harm categories
func safetySettings(threshold genai.HarmBlockThreshold) (settings []*genai.SafetySetting) {
	for _, category := range []genai.HarmCategory{
		/*
			// categories for PaLM 2 (Legacy) models
			genai.HarmCategoryUnspecified,
			genai.HarmCategoryDerogatory,
			genai.HarmCategoryToxicity,
			genai.HarmCategoryViolence,
			genai.HarmCategorySexual,
			genai.HarmCategoryMedical,
			genai.HarmCategoryDangerous,
		*/

		// all categories supported by Gemini models
		genai.HarmCategoryHarassment,
		genai.HarmCategoryHateSpeech,
		genai.HarmCategorySexuallyExplicit,
		genai.HarmCategoryDangerousContent,
	} {
		settings = append(settings, &genai.SafetySetting{
			Category:  category,
			Threshold: threshold,
		})
	}

	return settings
}

// generate user's name
func userName(user *tg.User) string {
	if user.Username != nil {
		return fmt.Sprintf("@%s (%s)", *user.Username, user.FirstName)
	} else {
		return user.FirstName
	}
}

// generate user's name from update
func userNameFromUpdate(update tg.Update) string {
	if user := update.GetFrom(); user != nil {
		return userName(user)
	}

	logInfo("there was no `from` in `update`")

	return "unknown"
}

// save prompt and its result to logs database
func savePromptAndResult(db *Database, chatID, userID int64, username string, prompt string, promptTokens int, resultTokens int, resultSuccessful bool) {
	if db != nil {
		if err := db.SavePrompt(Prompt{
			ChatID:   chatID,
			UserID:   userID,
			Username: username,
			Text:     prompt,
			Tokens:   promptTokens,
			Result: ParsedItem{
				Successful: resultSuccessful,
				Tokens:     resultTokens,
			},
		}); err != nil {
			log.Printf("failed to save prompt & result to database: %s", err)
		}
	}
}

// generate a help message with version info
func helpMessage() string {
	return fmt.Sprintf(msgHelp, version.Build(version.OS|version.Architecture|version.Revision))
}

// return a /start command handler
func startCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID

			send(b, conf, db, msgStart, chatID, nil)
		}
	}
}

// return a /list command handler
func listRemindersCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			var msg string
			chatID := message.Chat.ID
			options := tg.OptionsSendMessage{}.
				SetReplyMarkup(defaultReplyMarkup())

			if reminders, err := db.UndeliveredQueueItems(chatID); err == nil {
				if len(reminders) > 0 {
					format := fmt.Sprintf("%s\n", msgListItemFormat)
					for _, r := range reminders {
						msg += fmt.Sprintf(format, datetimeToStr(r.FireOn), r.Message)
					}
				} else {
					msg = msgNoReminders
				}
			} else {
				logError(db, "failed to process %s: %s", cmdListReminders, err)
			}

			// send message
			if len(msg) <= 0 {
				msg = msgError
			}
			if sent := b.SendMessage(chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /cancel command handler
func cancelCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			var msg string
			chatID := message.Chat.ID
			options := tg.OptionsSendMessage{}.
				SetReplyMarkup(defaultReplyMarkup())

			if reminders, err := db.UndeliveredQueueItems(chatID); err == nil {
				if len(reminders) > 0 {
					// inline keyboards
					keys := make(map[string]string)
					for _, r := range reminders {
						keys[fmt.Sprintf(msgListItemFormat, datetimeToStr(r.FireOn), r.Message)] = fmt.Sprintf("%s %d", cmdCancel, r.ID)
					}
					buttons := tg.NewInlineKeyboardButtonsAsRowsWithCallbackData(keys)

					// add a cancel button for canceling reminder
					buttons = append(buttons, []tg.InlineKeyboardButton{
						tg.NewInlineKeyboardButton(msgCancel).
							SetCallbackData(cmdCancel),
					})

					// options
					options.SetReplyMarkup(tg.NewInlineKeyboardMarkup(buttons))

					msg = msgCancelWhat
				} else {
					msg = msgNoReminders
				}
			} else {
				logError(db, "failed to process %s: %s", cmdCancel, err)
			}

			// send message
			if len(msg) <= 0 {
				msg = msgError
			}
			if sent := b.SendMessage(chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /stats command handler
func statsCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("stats command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			var msg string
			if db == nil {
				msg = msgDatabaseNotConfigured
			} else {
				msg = db.Stats()
			}

			send(b, conf, db, msg, chatID, &messageID)
		}
	}
}

// return a /help command handler
func helpCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("help command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			send(b, conf, db, helpMessage(), chatID, &messageID)
		}
	}
}

// return a 'no such command' handler
func noSuchCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, cmd, args string) {
	return func(b *tg.Bot, update tg.Update, cmd, args string) {
		if !isAllowed(conf, update) {
			log.Printf("command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			send(b, conf, db, fmt.Sprintf(msgCmdNotSupported, cmd), chatID, &messageID)
		}
	}
}

var _stdout = log.New(os.Stdout, "", log.LstdFlags)
var _stderr = log.New(os.Stderr, "", log.LstdFlags)

// log info message
func logInfo(format string, a ...any) {
	_stdout.Printf(format, a...)
}

// log debug message (printed to stdout only when `IsVerbose` is true)
func logDebug(conf config, format string, a ...any) {
	if conf.Verbose {
		_stdout.Printf(format, a...)
	}
}

// log error message
func logError(db *Database, format string, a ...any) {
	if db != nil {
		db.LogError(format, a...)
	}

	_stderr.Printf(format, a...)
}

// log error message and exit(1)
func logErrorAndDie(db *Database, format string, a ...any) {
	if db != nil {
		db.LogError(format, a...)
	}

	_stderr.Fatalf(format, a...)
}

// default reply markup
func defaultReplyMarkup() tg.ReplyKeyboardMarkup {
	return tg.NewReplyKeyboardMarkup( // show keyboards
		[][]tg.KeyboardButton{
			tg.NewKeyboardButtons(cmdListReminders, cmdCancel, cmdStats, cmdHelp),
		}).
		SetResizeKeyboard(true)
}

// generate inline keyboard buttons for multiple datetimes
func datetimeButtonsForCallbackQuery(items []parsedItem, chatID int64, messageID int64) [][]tg.InlineKeyboardButton {
	// datetime buttons
	keys := make(map[string]string)

	var title, generated string
	for _, item := range items {
		if item.Generated {
			generated = " *"
		} else {
			generated = ""
		}
		title = fmt.Sprintf("%s%s", datetimeToStr(item.When), generated)
		keys[title] = fmt.Sprintf("%s %d/%d/%s", cmdLoad, chatID, messageID, datetimeToStr(item.When))
	}
	buttons := tg.NewInlineKeyboardButtonsAsRowsWithCallbackData(keys)

	// add cancel button
	buttons = append(buttons, []tg.InlineKeyboardButton{
		tg.NewInlineKeyboardButton(msgCancel).
			SetCallbackData(cmdCancel),
	})

	return buttons
}

// format given time to string
func datetimeToStr(t time.Time) string {
	return t.In(_location).Format(datetimeFormat)
}
