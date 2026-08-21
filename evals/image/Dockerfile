FROM golang:1.26.5-bookworm AS forest-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/forest .

FROM node:22.23.1-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git jq python3 sudo \
    && rm -rf /var/lib/apt/lists/*
RUN npm install --global @earendil-works/pi-coding-agent@0.84.1
COPY --from=forest-build /out/forest /usr/local/bin/forest
COPY agents /opt/iron-forest/agents
COPY evals/runtime /opt/iron-forest-eval
RUN useradd --create-home forest \
    && mkdir -p /hidden /workspace /run/forest-eval \
    && chmod 0700 /hidden /opt/iron-forest-eval \
    && chmod 0755 /workspace /run/forest-eval \
    && chown forest:forest /workspace \
    && chmod 0755 /opt/iron-forest-eval/gh /opt/iron-forest-eval/powder /opt/iron-forest-eval/race.py /opt/iron-forest-eval/setup.py /opt/iron-forest-eval/reference.py \
    && printf '%s\n' 'forest ALL=(root) NOPASSWD: /usr/bin/python3 /opt/iron-forest-eval/setup.py /solution/scenario.json, /usr/bin/python3 /opt/iron-forest-eval/reference.py /solution/scenario.json, /opt/iron-forest-eval/gh, /opt/iron-forest-eval/powder, /opt/iron-forest-eval/race.py' > /etc/sudoers.d/forest-eval \
    && chmod 0440 /etc/sudoers.d/forest-eval \
    && cp /opt/iron-forest-eval/git /usr/local/bin/git \
    && cp /opt/iron-forest-eval/gh-proxy /usr/local/bin/gh \
    && cp /opt/iron-forest-eval/powder-proxy /usr/local/bin/powder \
    && chmod 0755 /usr/local/bin/git /usr/local/bin/gh /usr/local/bin/powder
ENV HOME=/home/forest
WORKDIR /workspace
