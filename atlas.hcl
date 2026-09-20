variable "url" {
  type    = string
  default = getenv("DATABASE_URL")
}

env "local" {
  url = var.url
  
  dev = "docker://postgres/17/dev?search_path=public"

  migration {
    dir = "file://db/migrations"
  }
}
