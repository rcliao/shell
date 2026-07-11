package telegram

import "testing"

func TestClassifyGroupDomain(t *testing.T) {
	cases := []struct{ msg, want string }{
		// practical (default): all task/info/product — no keyword enumeration needed
		{"這個玻璃鍋蓋能進洗碗機嗎？", DomainPractical},
		{"巴西木葉子黃了怎麼辦", DomainPractical},
		{"SKIN1004 精華什麼時候用", DomainPractical},
		{"what time is the game tonight", DomainPractical},
		// companionship: feelings/mood
		{"今天有點累", DomainCompanionship},
		{"我心情不太好，想聊聊", DomainCompanionship},
		{"i'm feeling really tired today", DomainCompanionship},
		// social: greetings / reactions / plural address → both stay present
		{"hi babies", DomainSocial},
		{"哈哈哈太好笑了", DomainSocial},
		{"ahhh", DomainSocial},
		{"你們今天乖不乖", DomainSocial},
	}
	for _, c := range cases {
		if got := ClassifyGroupDomain(c.msg); got != c.want {
			t.Errorf("ClassifyGroupDomain(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
