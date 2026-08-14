FROM golang:1.26.5-bookworm AS forest-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/forest .

FROM node:22.23.1-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git jq python3 \
    && rm -rf /var/lib/apt/lists/*
RUN npm install --global @earendil-works/pi-coding-agent@0.84.1
COPY --from=forest-build /out/forest /usr/local/bin/forest
COPY agents /opt/iron-forest/agents
COPY evals/runtime /opt/iron-forest-eval
RUN chmod +x /opt/iron-forest-eval/gh /opt/iron-forest-eval/git \
    && ln -s /opt/iron-forest-eval/gh /usr/local/bin/gh \
    && ln -s /opt/iron-forest-eval/git /usr/local/bin/git
ENV HOME=/root
WORKDIR /workspace
