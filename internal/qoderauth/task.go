package qoderauth

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

/**
 * @brief cosyTask holds the business identity shared by a user prompt's tool loop.
 */
type cosyTask struct {
	ID    string
	Name  string
	Begin int64
	Stage string
	used  time.Time
	key   [32]byte
}

/**
 * @brief cosyTasks retains at most 512 recently used tasks for two hours each.
 */
var cosyTasks = struct {
	sync.Mutex
	entries map[[32]byte]*list.Element
	order   *list.List
}{entries: make(map[[32]byte]*list.Element), order: list.New()}

/**
 * @brief cosySession hashes the explicit session or the conversation's opening turn.
 * @param input Logical Chat session fields.
 * @param messages Original conversation history.
 * @return A stable UUID-shaped session digest.
 */
func cosySession(input map[string]json.RawMessage, messages []map[string]json.RawMessage) string {
	seed := ""
	for _, field := range []struct{ key, prefix string }{{"session_id", "session"}, {"prompt_cache_key", "key"}, {"user", "user"}} {
		if value := cosyString(input[field.key]); strings.TrimSpace(value) != "" {
			seed = field.prefix + "\x00" + value
			break
		}
	}
	if seed == "" {
		systems := []string{}
		first := json.RawMessage(`""`)
		found := false
		for _, message := range messages {
			switch cosyString(message["role"]) {
			case "system", "developer":
				systems = append(systems, cosyContentText(message["content"]))
			case "user":
				if !found {
					first, found = message["content"], true
				}
			}
		}
		opening, _ := json.Marshal([]any{systems, first})
		seed = "chat\x00" + string(opening)
	}
	digest := sha256.Sum256([]byte(seed))
	hexID := hex.EncodeToString(digest[:16])
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
}

/**
 * @brief cosyTaskFor returns a snapshot so concurrent requests never share mutable state.
 * @param c Account identity used to isolate task state.
 * @param session Hashed conversation session.
 * @param messages Original conversation history.
 * @return An immutable snapshot of the current task.
 */
func cosyTaskFor(c *Credential, session string, messages []map[string]json.RawMessage) cosyTask {
	at := -1
	var prompt json.RawMessage
	processing := false
	for i, message := range messages {
		switch cosyString(message["role"]) {
		case "user":
			at, prompt, processing = i, message["content"], false
		case "assistant":
			processing = true
		}
	}
	identity, _ := json.Marshal([]any{c.Region, c.UserID, session, at, prompt})
	key := sha256.Sum256(identity)
	now := time.Now()
	cosyTasks.Lock()
	defer cosyTasks.Unlock()
	for element := cosyTasks.order.Front(); element != nil; element = cosyTasks.order.Front() {
		task := element.Value.(cosyTask)
		if now.Sub(task.used) <= 2*time.Hour {
			break
		}
		delete(cosyTasks.entries, task.key)
		cosyTasks.order.Remove(element)
	}
	element := cosyTasks.entries[key]
	if element == nil {
		name := []rune(cosyContentText(prompt))
		task := cosyTask{ID: uuid.NewString(), Name: string(name[:min(len(name), 10)]), Begin: now.UnixMilli(), Stage: "start", key: key}
		element = cosyTasks.order.PushBack(task)
		cosyTasks.entries[key] = element
	}
	task := element.Value.(cosyTask)
	task.used = now
	task.Stage = "start"
	if processing {
		task.Stage = "processing"
	}
	element.Value = task
	cosyTasks.order.MoveToBack(element)
	if cosyTasks.order.Len() > 512 {
		oldest := cosyTasks.order.Front()
		delete(cosyTasks.entries, oldest.Value.(cosyTask).key)
		cosyTasks.order.Remove(oldest)
	}
	return task
}
