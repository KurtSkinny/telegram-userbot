package filters_test

import (
	"reflect"
	"testing"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/infra/config"
)

func TestMatchMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		text   string
		filter config.Filter
		want   filters.Result
	}{
		{
			name: "keywordsAllAndAnyAndRegexMatched",
			text: "Просто-прелложение, где Нет! #НиЧеГо!таКого!",
			filter: config.Filter{
				Match: config.Match{
					KeywordsAny: []string{"просто", "предложение"},
					KeywordsAll: []string{"где", "нет", "#ничего"},
					Regex:       `(?i)такого`,
				},
			},
			want: filters.Result{
				Matched:    true,
				Keywords:   []string{"где", "нет", "#ничего", "просто"},
				RegexMatch: "таКого",
			},
		},
		{
			name: "keywordsAllAndAnyAndRegexMatchedExclude",
			text: "Просто-прелложение, где Нет! #НиЧеГо!такого!",
			filter: config.Filter{
				Match: config.Match{
					KeywordsAny:        []string{"просто", "предложение"},
					KeywordsAll:        []string{"где", "нет"},
					Regex:              `(?i)ничего`,
					ExcludeKeywordsAny: []string{"такого"},
				},
			},
			want: filters.Result{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := filters.MatchMessage(tc.text, tc.filter)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MatchMessage() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
