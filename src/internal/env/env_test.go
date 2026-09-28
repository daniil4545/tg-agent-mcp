package env

import "testing"

// Пустая переменная и мусор лечатся по-разному: первое даёт дефолт, второе
// обязано уронить конфигурацию. Тест держит именно эту границу.
func TestIntRejectsNonPositiveAndKeepsFallback(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  int
		fails bool
	}{
		{"пусто даёт дефолт", "", 7, false},
		{"значение перебивает дефолт", "3", 3, false},
		{"ноль это не значение", "0", 0, true},
		{"отрицательное", "-1", 0, true},
		{"не число", "many", 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LIMIT", c.raw)
			got, err := Int("LIMIT", 7)
			if c.fails {
				if err == nil {
					t.Fatalf("LIMIT=%q принят, ожидалась ошибка", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("LIMIT=%q: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("LIMIT=%q дал %d, ожидалось %d", c.raw, got, c.want)
			}
		})
	}
}

func TestInt64ListRejectsPartialGarbage(t *testing.T) {
	t.Setenv("IDS", "10, 20 ,30")
	got, err := Int64List("IDS")
	if err != nil {
		t.Fatalf("разбор списка: %v", err)
	}
	if len(got) != 3 || got[0] != 10 || got[2] != 30 {
		t.Fatalf("список разобран как %v", got)
	}

	// Один кривой элемент валит весь список: частично разобранный allowlist
	// опаснее пустого, потому что выглядит рабочим.
	t.Setenv("IDS", "10,abc,30")
	if _, err := Int64List("IDS"); err == nil {
		t.Fatal("список с мусором принят")
	}
}

func TestProxyURLRejectsUnsupportedScheme(t *testing.T) {
	t.Setenv("PROXY", "")
	parsed, err := ProxyURL("PROXY")
	if err != nil || parsed != nil {
		t.Fatalf("пустой прокси дал %v, %v", parsed, err)
	}

	t.Setenv("PROXY", "socks5://10.0.0.1:20170")
	if _, err := ProxyURL("PROXY"); err != nil {
		t.Fatalf("socks5 отвергнут: %v", err)
	}

	t.Setenv("PROXY", "ftp://10.0.0.1:21")
	if _, err := ProxyURL("PROXY"); err == nil {
		t.Fatal("ftp принят как прокси")
	}

	t.Setenv("PROXY", "10.0.0.1:20170")
	if _, err := ProxyURL("PROXY"); err == nil {
		t.Fatal("адрес без схемы принят")
	}
}

func TestRequireReportsMissingName(t *testing.T) {
	t.Setenv("TOKEN", "")
	_, err := RequireString("TOKEN")
	if err == nil {
		t.Fatal("пустая обязательная переменная принята")
	}
	// Имя переменной в тексте ошибки - единственное, по чему в логе старта
	// видно, чего не хватает.
	if got := err.Error(); got != "TOKEN is required" {
		t.Fatalf("ошибка не называет переменную: %q", got)
	}
}
