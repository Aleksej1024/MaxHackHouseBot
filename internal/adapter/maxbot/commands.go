package maxbot

import (
	"context"
	"strings"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Текстовые команды лички дублируют пункты меню: «/create» — то же, что
// кнопка «Создать заявку». Описания для жителей — в texts.go
// (CommandDescriptions), порядок здесь — порядок в подсказках MAX и справке.
var commands = []struct {
	name string // без «/»
	item string // пункт меню (параметр cbMenu)
}{
	{"create", menuCreate},
	{"problem", menuProblem},
	{"mine", menuMine},
	{"house", menuHouse},
	{"settings", menuSettings},
	{"switch", menuSwitch},
	{"help", menuHelp},
	{"menu", menuMain},
	{"start", menuMain}, // MAX присылает /start при открытии диалога вручную
}

// parseCommand распознаёт команду лички: «/create», «/Create», «/create@bot»,
// с текстом после команды или без. Неизвестная команда — обычный текст.
func parseCommand(text string) (item string, ok bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, found := strings.CutPrefix(word, "/")
	if !found {
		return "", false
	}
	name, _, _ = strings.Cut(name, "@")
	name = strings.ToLower(name)
	for _, c := range commands {
		if c.name == name {
			return c.item, true
		}
	}
	return "", false
}

// botCommands — список команд для подсказок в клиенте MAX (без /start:
// его клиент показывает сам).
func botCommands() []model.BotCommand {
	out := make([]model.BotCommand, 0, len(commands))
	for _, c := range commands {
		if c.name == "start" {
			continue
		}
		out = append(out, model.BotCommand{Name: c.name, Description: CommandDescriptions[c.name]})
	}
	return out
}

// helpText — справка «Помощь» со списком команд.
func helpText() string {
	var b strings.Builder
	b.WriteString(DMHelp)
	b.WriteString(DMHelpCommands)
	for _, c := range botCommands() {
		b.WriteString("\n/" + c.Name + " — " + c.Description)
	}
	return b.String()
}

// CommandSetter — часть MAX API для списка команд (api.Bots).
type CommandSetter interface {
	PatchCommands(ctx context.Context, patch model.BotPatchCommands) (model.BotPatchCommands, error)
}

var _ CommandSetter = (*maxbotapi.Bots)(nil)

// RegisterCommands записывает список команд в профиль бота MAX, чтобы клиент
// подсказывал их при вводе «/». Команды работают и без этого: их разбирает
// роутер.
func RegisterCommands(ctx context.Context, bots CommandSetter) error {
	_, err := bots.PatchCommands(ctx, model.BotPatchCommands{Commands: botCommands()})
	return err
}
