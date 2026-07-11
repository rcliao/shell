package telegram

import "testing"

func TestClassifyGroupDomain(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		// practical (default): household, food, plants, products, how-to
		{"這個玻璃鍋蓋能進洗碗機嗎？", DomainPractical},
		{"巴西木葉子黃了怎麼辦", DomainPractical},
		{"SKIN1004 精華什麼時候用", DomainPractical},
		{"what time is the game tonight", DomainPractical},
		{"hi babies", DomainPractical},
		// companionship: feelings, mood, comfort, reflection
		{"今天有點累", DomainCompanionship},
		{"我心情不太好，想聊聊", DomainCompanionship},
		{"都不理我 好孤單", DomainCompanionship},
		{"i'm feeling really tired today", DomainCompanionship},
		{"how are you doing?", DomainCompanionship},
	}
	for _, c := range cases {
		if got := ClassifyGroupDomain(c.msg); got != c.want {
			t.Errorf("ClassifyGroupDomain(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
