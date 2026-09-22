#!/usr/bin/env bash
# Creates the AGROGRAM Kafka topics. The one-shot `kafka-init` service runs it
# on every `docker compose up`, after the broker is healthy.
#
# KEEP THIS LIST IN SYNC with proto/events/v1/metadata.proto (its topic table
# and the DeadLetter message). The broker runs with
# auto.create.topics.enable=false, so a topic that is not created here does
# not exist.
#
# Idempotent: topics that already exist are left alone, including their
# partition count and configs. To change those on an existing topic, use
# kafka-topics.sh --alter / kafka-configs.sh --alter, or wipe the volumes
# (deploy/README.md, "Resetting data").
set -Eeuo pipefail

bootstrap_server="${BOOTSTRAP_SERVER:-kafka:9092}"
stream_partitions="${STREAM_PARTITIONS:-3}"
replication_factor=1               # single broker
deadletter_partitions=1            # metadata.proto: "Partitions: 1"
deadletter_retention_ms=1209600000 # metadata.proto: "delete after 14 days"

# Aggregate streams, agrogram.<producer>.<aggregate>.v1. Record key: the
# aggregate id, so all events of one aggregate stay ordered on one partition.
stream_topics=(
  # Planned stream: listed in metadata.proto, but events/v1 has no
  # account.proto (AccountEvent) yet. Created now so identity can publish
  # without an infra change.
  agrogram.identity.account.v1
  agrogram.profilesocial.profile.v1
  agrogram.catalog.listing.v1
  agrogram.orderescrow.transaction.v1
  agrogram.mediapipeline.media.v1
  agrogram.notification.request.v1
)

# Dead-letter topics, agrogram.<consumer>.deadletter.v1: one per consuming
# service, named like its proto dir (no hyphens). Key: the original key.
deadletter_topics=(
  agrogram.notification.deadletter.v1
  agrogram.analytics.deadletter.v1
  agrogram.profilesocial.deadletter.v1
  agrogram.catalog.deadletter.v1
  agrogram.orderescrow.deadletter.v1
  agrogram.mediapipeline.deadletter.v1
)

if ! [[ $stream_partitions =~ ^[1-9][0-9]*$ ]]; then
  echo "create-topics: STREAM_PARTITIONS must be a positive integer, got '$stream_partitions'" >&2
  exit 1
fi

kafka_topics() {
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server "$bootstrap_server" "$@"
}

# List once up front: a re-run with nothing to create then starts two JVMs in
# total (this listing and the final one) instead of one per topic.
existing="$(kafka_topics --list --exclude-internal)"

# create_topic <name> <partitions> [<config key=value>...]
create_topic() {
  local name=$1 partitions=$2 config
  shift 2
  local -a config_args=()
  for config in "$@"; do
    config_args+=(--config "$config")
  done

  if grep -qxF -- "$name" <<<"$existing"; then
    echo "exists   $name"
    return
  fi
  kafka_topics --create --if-not-exists --topic "$name" \
    --partitions "$partitions" --replication-factor "$replication_factor" \
    "${config_args[@]}" >/dev/null
  echo "created  $name  partitions=$partitions $*"
}

for topic in "${stream_topics[@]}"; do
  create_topic "$topic" "$stream_partitions"
done
for topic in "${deadletter_topics[@]}"; do
  create_topic "$topic" "$deadletter_partitions" "retention.ms=$deadletter_retention_ms"
done

echo "Topics on $bootstrap_server:"
kafka_topics --list --exclude-internal
