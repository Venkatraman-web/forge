# Atlas configuration. GORM describes the desired schema; Atlas turns changes to
# it into versioned SQL files in migrations/ and applies them.

# Runs the GORM provider (pinned in go.mod as a tool). It reads the JobRecord
# struct in internal/store and prints the desired schema as SQL DDL.
data "external_schema" "gorm" {
  program = [
    "go", "tool", "atlas-provider-gorm",
    "load",
    "--path", "./internal/store",
    "--dialect", "postgres",
  ]
}

env "local" {
  # Desired schema: whatever the GORM model says right now.
  src = data.external_schema.gorm.url

  # Disposable, empty PostgreSQL that Atlas starts in Docker only to plan diffs.
  # It is never your project database and is removed afterwards.
  dev = "docker://postgres/16/dev?search_path=public"

  migration {
    dir = "file://migrations"
  }
}
