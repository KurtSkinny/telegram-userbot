package filters_test

import (
	"testing"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/infra/config"
)

// хелпер для сборки фильтра
func F(
	chats []int64,
	re string,
	all []string,
	anny []string,
	exclAny []string,
	exclRe string,
) config.Filter {
	return config.Filter{
		Chats: chats,
		Match: config.Match{
			Regex:              re,
			KeywordsAll:        all,
			KeywordsAny:        anny,
			ExcludeKeywordsAny: exclAny,
			ExcludeRegex:       exclRe,
		},
	}
}

func TestMatchMessage_CorePipeline(t *testing.T) {
	type TC struct {
		name   string
		text   string
		filter config.Filter
		ok     bool
		wantKW []string // ожидаемые совпавшие ключи (порядок важен: All затем Any)
		wantRe string   // ожидаемый фрагмент регекспа
	}
	tests := []TC{
		{
			name:   "Пустые условия — матчится по умолчанию",
			text:   "",
			filter: F(nil, "", nil, nil, nil, ""),
			ok:     true,
		},
		{
			name:   "Include Regex (подстрока) — простое совпадение",
			text:   "привет, мир!",
			filter: F(nil, "мир", nil, nil, nil, ""),
			ok:     true,
			wantRe: "мир",
		},
		{
			name:   "Include Regex — нет совпадения",
			text:   "привет, мир!",
			filter: F(nil, "пока", nil, nil, nil, ""),
			ok:     false,
		},
		{
			name:   "Include KeywordsAll — все слова есть (регистр игнорируется, кириллица)",
			text:   "Привет, мир. Голос бота слышен.",
			filter: F(nil, "", []string{"привет", "мир"}, nil, nil, ""),
			ok:     true,
			wantKW: []string{"привет", "мир"},
		},
		{
			name:   "Include KeywordsAll — одного слова нет",
			text:   "привет, люди",
			filter: F(nil, "", []string{"привет", "мир"}, nil, nil, ""),
			ok:     false,
		},
		{
			name:   "Include KeywordsAny — одно из слов есть",
			text:   "запуск двигателя прошёл штатно",
			filter: F(nil, "", nil, []string{"авария", "запуск"}, nil, ""),
			ok:     true,
			wantKW: []string{"запуск"},
		},
		{
			name:   "Include KeywordsAny — ни одного совпадения",
			text:   "система в норме",
			filter: F(nil, "", nil, []string{"авария", "паника"}, nil, ""),
			ok:     false,
		},
		{
			name:   "Exclude KeywordsAny — запрет срабатывает",
			text:   "внимание: авария в цехе",
			filter: F(nil, "", nil, []string{"внимание"}, []string{"авария"}, ""),
			ok:     false,
		},
		{
			name:   "Exclude Regex — запрет по регекспу",
			text:   "CARD 4111-1111-1111-1111",
			filter: F(nil, "", nil, []string{"card"}, nil, `\d{4}-\d{4}-\d{4}-\d{4}`),
			ok:     false,
		},
		{
			name:   "Границы слов — 'кот' не матчится в 'котелок', но матчится в 'кот, ...'",
			text:   "котелок медный, кот, кот-рыбак",
			filter: F(nil, "", []string{"кот"}, nil, nil, ""),
			ok:     true,
			// Внутри includeKeywordsAll используется ContainsSmart с границами слов:
			// "котелок" — не матчится; "кот," и "кот-рыбак" — матчится.
			wantKW: []string{"кот"},
		},
		{
			name:   "Спецсимволы — 'C++' находится как слово",
			text:   "учим C++ для начинающих",
			filter: F(nil, "", []string{"C++"}, nil, nil, ""),
			ok:     true,
			wantKW: []string{"C++"},
		},
		{
			name:   "Include Regex против Exclude Regex — запретный регексп побеждает",
			text:   "допуск 404 на стороне клиента",
			filter: F(nil, `\d+`, nil, nil, nil, `^.*404.*$`),
			ok:     false,
		},
		{
			name:   "Битый include-regex — ошибка компиляции приводит к отклонению",
			text:   "что угодно",
			filter: F(nil, "(", nil, nil, nil, ""),
			ok:     false,
		},
		{
			name:   "Битый exclude-regex — тоже отклоняем",
			text:   "что угодно",
			filter: F(nil, "", nil, nil, nil, "("),
			ok:     false,
		},
		{
			name: "Комбинация: includeRegex + All + Any — всё прошло, нет запретов",
			text: "Заявка 123 принята. Срочно обработать!",
			filter: F(nil,
				`\d+`,                       // includeRegex найдёт "123"
				[]string{"заявка"},          // all
				[]string{"срочно", "позже"}, // any
				nil,
				"",
			),
			ok:     true,
			wantKW: []string{"заявка", "срочно"},
			wantRe: "123",
		},
	}

	for _, tc := range tests {
		ctc := tc
		t.Run(ctc.name, func(t *testing.T) {
			assertMatchCase(t, ctc)
		})
	}
}

func assertMatchCase(t *testing.T, tc struct {
	name   string
	text   string
	filter config.Filter
	ok     bool
	wantKW []string
	wantRe string
}) {
	t.Helper()
	got := filters.MatchMessage(tc.text, tc.filter)
	if got.Matched != tc.ok {
		t.Fatalf(
			"Matched=%v, want %v (text=%q, filter=%+v)",
			got.Matched,
			tc.ok,
			tc.text,
			tc.filter,
		)
	}
	if !got.Matched {
		return
	}
	if tc.wantKW != nil {
		if len(got.Keywords) != len(tc.wantKW) {
			t.Fatalf(
				"Keywords len=%d, want %d; got=%v want=%v",
				len(got.Keywords),
				len(tc.wantKW),
				got.Keywords,
				tc.wantKW,
			)
		}
		for i := range tc.wantKW {
			if got.Keywords[i] != tc.wantKW[i] {
				t.Fatalf(
					"Keywords[%d]=%q, want %q; got=%v want=%v",
					i,
					got.Keywords[i],
					tc.wantKW[i],
					got.Keywords,
					tc.wantKW,
				)
			}
		}
	}
	if tc.wantRe != "" && got.RegexMatch != tc.wantRe {
		t.Fatalf(
			"RegexMatch=%q, want %q",
			got.RegexMatch,
			tc.wantRe,
		)
	}
}

func TestContainsSmart_BoundariesAndCase(t *testing.T) {
	type TC struct {
		text string
		kw   string
		want bool
	}
	cases := []TC{
		{"foo-bar", "foo", true},        // граница слов по небуквенному символу
		{"foobar", "foo", false},        // внутри слова — не должно матчиться
		{"Привет, мир", "привет", true}, // кириллица + регистр
		{"C++ guide", "C++", true},      // спецсимволы
		{"email: a.b@c", "b@c", true},   // границы по краям ключа: '.' слева и конец строки справа — матчится
		{"", "что-то", false},
		{"abc", "", false},
	}
	for i, c := range cases {
		got := filters.ContainsSmart(c.text, c.kw)
		if got != c.want {
			t.Fatalf(
				"[%d] ContainsSmart(%q, %q)=%v, want %v",
				i,
				c.text,
				c.kw,
				got,
				c.want,
			)
		}
	}
}
