APP := ipsc2mmdvm

.PHONY: build web-build yarn-install test clean

build: web-build
	go build -o $(APP) .

web-build:
	cd webapp && yarn install
	cd webapp && yarn build

yarn-install:
	cd webapp && yarn install

test:
	go test ./...

clean:
	rm -f $(APP)
	rm -rf internal/web/dist/assets
