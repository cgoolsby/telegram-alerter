IMAGE ?= telegram-alerter:latest

.PHONY: build test docker-build docker-push deploy

build:
	CGO_ENABLED=0 go build -o telegram-alerter .

test:
	go test ./...

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	kubectl apply -f k8s/namespace.yaml
	kubectl apply -f k8s/deployment.yaml -f k8s/service.yaml
