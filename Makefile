.PHONY: build run now up down logs

build:
	docker compose build

run:
	docker compose up -d

now:
	docker compose run --rm email-agent --config config.yaml --prefs preferences.yaml --now

down:
	docker compose down

logs:
	docker compose logs -f
