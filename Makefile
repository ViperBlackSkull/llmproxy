.PHONY: build clean

build:
	go build -o llmproxyd .

clean:
	rm -f llmproxyd
	rm -rf logs/
