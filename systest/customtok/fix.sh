#!/bin/bash

for FILE in */*.go; do
	DIR=`dirname $FILE`
	NEWFILE=`sed s/main/$DIR/g <(echo $FILE)`
	sed -i s"/package main/package $DIR/g" $FILE
	mv $FILE $NEWFILE
done
