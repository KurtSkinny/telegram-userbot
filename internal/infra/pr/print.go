// Package pr — тонкая обёртка для унифицированного вывода в интерактивной CLI-среде.
// Инициализирует readline с отменяемым stdin, переназначает stdout/stderr на его буферы
// и предоставляет удобные функции печати для обычного и диагностического вывода.
// Конкурентность: мьютекс защищает только смену целевых writer’ов; сами записи в writer
// не сериализуются здесь и должны быть потокобезопасны на стороне целевого writer’а.

package pr

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/chzyer/readline"
	"github.com/kr/pretty"
)

var (
	// rl — активный инстанс readline. Появляется после Init(). Может быть nil до инициализации.
	rl *readline.Instance
	// out — текущий поток стандартного вывода. До Init() указывает на os.Stdout; после Init() — на rl.Stdout().
	out io.Writer = os.Stdout
	// errOut — поток вывода ошибок. До Init() — os.Stderr; после Init() — rl.Stderr().
	errOut io.Writer = os.Stderr
	// mu защищает замену ссылок на writer’ы и cancelableIn. Не сериализует сами операции записи.
	mu sync.Mutex

	// cancelableIn — дескриптор stdin, который можно закрыть для прерывания чтения (io.EOF в readline).
	// Инициализируется в Init() через readline.NewCancelableStdin.
	cancelableIn interface{ Close() error }
)

// Init настраивает readline и перенаправляет внутренние потоки вывода на его stdout/stderr.
// Использует cancelable stdin, чтобы прервать ожидание ввода при shutdown. Повторный вызов не предусмотрен.
func Init() error {
	// Создаём отменяемый stdin: закрытие cs приведёт к io.EOF у readline и аккуратному выходу из ожидания ввода.
	cs := readline.NewCancelableStdin(os.Stdin)
	// Минимальная конфигурация: используем только переопределённый Stdin.
	newRl, err := readline.NewEx(&readline.Config{Stdin: cs})
	if err != nil {
		_ = cs.Close()
		return err
	}
	rl = newRl

	mu.Lock()
	cancelableIn = cs
	// Переназначаем целевые writer’ы на буферы readline.
	out = rl.Stdout()
	errOut = rl.Stderr()
	mu.Unlock()

	return nil
}

// InterruptReadline закрывает cancelable stdin: Readline() получает io.EOF и возвращается.
// Идемпотентна: повторное закрытие проигнорируется реализацией.
func InterruptReadline() {
	if cancelableIn != nil {
		_ = cancelableIn.Close()
	}
}

// SetPrompt задаёт строку приглашения. Предполагает, что Init() уже был вызван.
// TODO: добавить проверку rl на nil и безопасный no-op, если прому некуда выставлять.
func SetPrompt(prompt string) {
	rl.SetPrompt(prompt)
}

// Rl возвращает текущий инстанс readline (может быть nil, если Init() не вызывался).
func Rl() *readline.Instance {
	return rl
}

// Stdout возвращает текущий writer стандартного вывода. Блокировка защищает только чтение ссылки.
// Потокобезопасность самих записей зависит от реализации writer’а (rl.Stdout безопасен для параллельных вызовов).
func Stdout() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	return out
}

// Stderr возвращает текущий writer ошибок. Аналогично Stdout: защита только на чтение ссылки.
func Stderr() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	return errOut
}

// Print печатает значения в Stdout без перевода строки. Не накладывает доп. синхронизации вокруг записи.
func Print(a ...any) {
	fmt.Fprint(Stdout(), a...)
}

// Println печатает значения в Stdout и добавляет перевод строки. Работает и до Init(), используя os.Stdout.
func Println(a ...any) {
	fmt.Fprintln(Stdout(), a...)
}

// Printf форматирует строку и печатает её в Stdout. Для горячих путей предпочитайте заранее собранные строки.
func Printf(format string, a ...any) {
	fmt.Fprintf(Stdout(), format, a...)
}

// ErrPrint печатает значения в Stderr без перевода строки.
func ErrPrint(a ...any) {
	fmt.Fprint(Stderr(), a...)
}

// ErrPrintln печатает значения в Stderr и добавляет перевод строки.
func ErrPrintln(a ...any) {
	fmt.Fprintln(Stderr(), a...)
}

// ErrPrintf форматирует строку и печатает её в Stderr.
func ErrPrintf(format string, a ...any) {
	fmt.Fprintf(Stderr(), format, a...)
}

// PP pretty-печатает значение в Stdout. Удобно для отладки; не используйте в горячих участках из-за аллокаций.
func PP(v any) {
	fmt.Fprintf(Stdout(), "%# v\n", pretty.Formatter(v))
}

// Pf возвращает pretty-строку значения. Полезно для логов и тестов; помните про аллокации.
func Pf(v any) string {
	return fmt.Sprintf("%# v\n", pretty.Formatter(v))
}
