# syntax=docker/dockerfile:1.7-labs

FROM golang:1.23

ENV AWS_ACCESS_KEY_ID=<CHANGEME>
ENV AWS_SECRET_ACCESS_KEY=<CHANGEME>
ENV AWS_ENDPOINT_URL_S3=https://fly.storage.tigris.dev
ENV AWS_REGION=auto


WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY --parents cmd/*/* ./
COPY --parents internal/*/* ./

RUN GOOS=linux go build -o /docker-go-sunlight ./cmd/sunlight/

CMD ["/docker-go-sunlight","-c","/app/config/sunlight.yaml"]
EXPOSE 443