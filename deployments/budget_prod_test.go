package deployments

// Production readiness (owner, 2026-11-07): the first deployment is production, in an account that also holds other
// resources. Rudolph gets its own monthly cost budget with email alerts, its tables are protected from deletion,
// and environment directories (account IDs, domain, token digests) can never be committed.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const santaAPIDir = "terraform_modules/santa_api"

func filesIn(files []tfFile, dir string) []tfFile {
	var out []tfFile
	for _, f := range files {
		if filepath.Dir(f.path) == filepath.Clean(dir) {
			out = append(out, f)
		}
	}
	return out
}

func variableDefault(t *testing.T, files []tfFile, name string) (string, bool) {
	t.Helper()
	vs := blocks(files, "variable", name)
	if len(vs) != 1 {
		t.Errorf("variable %q declared %d times, want once", name, len(vs))
		return "", false
	}
	d, ok := vs[0].Body.Attributes["default"]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(sourceOf(t, files, d.Expr)), true
}

func TestProd_RudolphHasAMonthlyCostBudget(t *testing.T) {
	module := filesIn(loadTerraform(t, santaAPIDir), santaAPIDir)

	if d, ok := variableDefault(t, module, "monthly_budget_usd"); !ok || d != "0" {
		t.Errorf("monthly_budget_usd default %q, want 0 (no budget unless set)", d)
	}
	if d, ok := variableDefault(t, module, "budget_alert_emails"); !ok || d != "[]" {
		t.Errorf("budget_alert_emails default %q, want []", d)
	}

	budgets := blocks(module, "resource", "aws_budgets_budget")
	if len(budgets) != 1 {
		t.Fatalf("found %d aws_budgets_budget resources in the santa_api module, want 1", len(budgets))
	}
	b := budgets[0]
	src := string(b.Body.Range().SliceBytes(srcOf(t, module, b)))
	count, ok := b.Body.Attributes["count"]
	if !ok || !strings.Contains(sourceOf(t, module, count.Expr), "var.monthly_budget_usd") {
		t.Error("the budget is not conditional on var.monthly_budget_usd")
	}
	for attr, want := range map[string]string{"budget_type": "COST", "time_unit": "MONTHLY", "limit_unit": "USD"} {
		a, ok := b.Body.Attributes[attr]
		if v, _ := literal(a); !ok || v != want {
			t.Errorf("budget %s = %q, want %q", attr, v, want)
		}
	}
	if a, ok := b.Body.Attributes["limit_amount"]; !ok || !strings.Contains(sourceOf(t, module, a.Expr), "var.monthly_budget_usd") {
		t.Error("budget limit_amount does not come from var.monthly_budget_usd")
	}
	if a, ok := b.Body.Attributes["name"]; !ok || !strings.Contains(sourceOf(t, module, a.Expr), "var.prefix") {
		t.Error("budget name does not include var.prefix")
	}
	// Only Rudolph's own costs: the account holds other resources. Every Rudolph resource carries the provider's
	// default tag Name = Rudolph (an activated cost allocation tag).
	if !strings.Contains(src, `"TagKeyValue"`) || !strings.Contains(src, `user:Name$Rudolph`) {
		t.Errorf("the budget is not filtered to the Name = Rudolph cost allocation tag:\n%s", src)
	}

	type note struct{ kind, threshold string }
	seen := map[note]bool{}
	for _, n := range nested(b, "notification") {
		kind, _ := literal(n.Body.Attributes["notification_type"])
		cmp, _ := literal(n.Body.Attributes["comparison_operator"])
		ttype, _ := literal(n.Body.Attributes["threshold_type"])
		th := ""
		if a, ok := n.Body.Attributes["threshold"]; ok {
			th = strings.TrimSpace(sourceOf(t, module, a.Expr))
		}
		subs, ok := n.Body.Attributes["subscriber_email_addresses"]
		if !ok || !strings.Contains(sourceOf(t, module, subs.Expr), "var.budget_alert_emails") {
			t.Errorf("notification %s %s%% does not go to var.budget_alert_emails", kind, th)
		}
		if cmp != "GREATER_THAN" || ttype != "PERCENTAGE" {
			t.Errorf("notification %s %s: comparison %q, threshold type %q; want GREATER_THAN and PERCENTAGE", kind, th, cmp, ttype)
		}
		seen[note{kind, th}] = true
	}
	for _, want := range []note{{"ACTUAL", "80"}, {"ACTUAL", "100"}, {"FORECASTED", "100"}} {
		if !seen[want] {
			t.Errorf("no %s notification at %s%%", want.kind, want.threshold)
		}
	}
}

func srcOf(t *testing.T, files []tfFile, b *hclsyntax.Block) []byte {
	t.Helper()
	for _, f := range files {
		if f.path == b.Range().Filename {
			return f.src
		}
	}
	t.Fatalf("source of %s not found", b.Range())
	return nil
}

func TestProd_TablesAreProtectedFromDeletion(t *testing.T) {
	all := loadTerraform(t, santaAPIDir)
	module := filesIn(all, santaAPIDir)
	if d, ok := variableDefault(t, module, "table_deletion_protection"); !ok || d != "true" {
		t.Errorf("table_deletion_protection default %q, want true", d)
	}
	tables := blocks(all, "resource", "aws_dynamodb_table")
	if len(tables) < 2 {
		t.Fatalf("found %d DynamoDB tables, want the store and the events table", len(tables))
	}
	for _, tb := range tables {
		a, ok := tb.Body.Attributes["deletion_protection_enabled"]
		if !ok {
			t.Errorf("aws_dynamodb_table.%s has no deletion_protection_enabled", tb.Labels[1])
			continue
		}
		if v := sourceOf(t, all, a.Expr); !strings.HasPrefix(strings.TrimSpace(v), "var.") {
			t.Errorf("aws_dynamodb_table.%s deletion_protection_enabled = %s, want it from a variable", tb.Labels[1], v)
		}
	}
}

func TestProd_ExampleEnvironmentSetsBudgetAndProtection(t *testing.T) {
	example := loadTerraform(t, filepath.Join("environments", "example"))
	var main string
	for _, m := range blocks(example, "module", "santa_api") {
		main = string(m.Body.Range().SliceBytes(srcOf(t, example, m)))
	}
	if main == "" {
		t.Fatal("the example environment has no module \"santa_api\"")
	}
	for _, v := range []string{"monthly_budget_usd", "budget_alert_emails", "table_deletion_protection"} {
		if !strings.Contains(main, v) {
			t.Errorf("the example environment does not pass %s to the module", v)
		}
		if len(blocks(example, "variable", v)) != 1 {
			t.Errorf("the example environment does not declare variable %s", v)
		}
	}
	raw, err := os.ReadFile(filepath.Join("environments", "example", "config.auto.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"monthly_budget_usd", "budget_alert_emails", "table_deletion_protection"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("config.auto.tfvars.json has no %q", k)
		}
	}
}

// Real environments hold the account ID, domain and token digests. new_env.sh writes a .gitignore of `*` into each,
// but a directory made by hand has none, so the repository's own .gitignore must ignore them too.
func TestProd_EnvironmentDirectoriesAreNeverCommitted(t *testing.T) {
	for path, ignored := range map[string]bool{
		"deployments/environments/prod/config.auto.tfvars.json":    true,
		"deployments/environments/prod/_backend.tf":                true,
		"deployments/environments/anything/terraform.tfstate":      true,
		"deployments/environments/example/config.auto.tfvars.json": false,
		"deployments/environments/example/main.tf":                 false,
	} {
		err := exec.Command("git", "-C", "..", "check-ignore", "-q", "--no-index", path).Run()
		if got := err == nil; got != ignored {
			t.Errorf("%s: ignored %v, want %v", path, got, ignored)
		}
	}
}
