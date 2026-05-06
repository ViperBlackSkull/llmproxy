.PHONY: build clean

build:
	go build -o llmproxyd .
	gcc -o lib/intercept lib/intercept.c

clean:
	rm -f llmproxyd lib/intercept
	rm -rf logs/
