# Atlas configuration for ForgeCart's own database. Plain versioned SQL files
# (no ORM model), so only a migration directory and a throwaway dev database
# are needed. Run from the repository root:
#   atlas migrate apply -c file://forgecart/atlas.hcl --env forgecart \
#     --url "$FORGECART_DATABASE_URL"
# The payment service has its own database and migration directory:
#   atlas migrate apply -c file://forgecart/atlas.hcl --env payments --url "$FORGECART_PAYMENTS_DATABASE_URL"
env "payments" {
  dev = "docker://postgres/16/dev?search_path=public"

  migration {
    dir = "file://forgecart/payments_migrations"
  }
}

env "forgecart" {
  dev = "docker://postgres/16/dev?search_path=public"

  migration {
    dir = "file://forgecart/migrations"
  }
}
