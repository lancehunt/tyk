#!/bin/bash
LISTEN_PORT=8080
USE_MONGO=""
REDIS_PORT=6379
REDIS_HOST="localhost"
REDIS_PASSWORD=""
TYK_GATEWAY_DOMAIN="tyk.local"
MONGO_URL="mongodb://localhost/tyk_analytics"

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

for i in "$@"
do
case $i in
	-l=*|--listenport=*)
    LISTEN_PORT="${i#*=}"
    shift # past argument=value
    ;;
    -a=*|--dashboard=*)
    USE_MONGO="${i#*=}"
    shift # past argument=value
    ;;
    -r=*|--redishost=*)
    REDIS_HOST="${i#*=}"
    shift # past argument=value
    ;;
    -p=*|--redisport=*)
    REDIS_PORT="${i#*=}"
    shift # past argument=value
    ;;
    -s=*|--redispass=*)
    REDIS_PASSWORD="${i#*=}"
    shift # past argument=value
    ;;
    -d=*|--domain=*)
    TYK_GATEWAY_DOMAIN="${i#*=}"
    shift # past argument=value
    ;;
    -m=*|--mongo=*)
    MONGO_URL="${i#*=}"
    shift # past argument=value
    ;;
    --default)
    DEFAULT=YES
    shift # past argument with no value
    ;;
    *)
            # unknown option
    ;;
esac
done

echo "Listen Port = ${LISTEN_PORT}"
echo "Redis Host  = ${REDIS_HOST}"
echo "Redis Port  = ${REDIS_PORT}"
echo "Redis PW    = ${REDIS_PASSWORD}"
echo "Domain      = ${TYK_GATEWAY_DOMAIN}"

if [ -n "$USE_MONGO" ];
	then
	echo "Use Mongo   = ${USE_MONGO}"
	echo "Mongo URL   = ${MONGO_URL}"
fi

# Set up the editing file
TEMPLATE_FILE="tyk.self_contained.conf"
if [ -n "$USE_MONGO" ];
	then
	echo "==> Setting up with Dashboard"
	TEMPLATE_FILE="tyk.with_dash.conf"
fi

cp $DIR/data/$TEMPLATE_FILE $DIR/tyk.conf

# Update variables
sed -i 's/LISTEN_PORT/'$LISTEN_PORT'/g' $DIR/tyk.conf
sed -i 's/REDIS_HOST/'$REDIS_HOST'/g' $DIR/tyk.conf
sed -i 's/REDIS_PORT/'$REDIS_PORT'/g' $DIR/tyk.conf
sed -i 's/REDIS_PASSWORD/'$REDIS_PASSWORD'/g' $DIR/tyk.conf
sed -i 's#MONGO_URL#'$MONGO_URL'#g' $DIR/tyk.conf
sed -i 's#TYK_GATEWAY_DOMAIN#'$TYK_GATEWAY_DOMAIN'#g' $DIR/tyk.conf

echo "==> File written to ./tyk.conf"
sudo cp $DIR/tyk.conf $DIR/../tyk.conf
echo "==> File copied to $DIR/../tyk.conf"