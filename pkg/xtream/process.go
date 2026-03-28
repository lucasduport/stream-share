/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */
package xtream

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// ProcessResponse processes various types of xtream-codes responses into generic maps/slices.
func ProcessResponse(resp interface{}) interface{} {
	if resp == nil {
		return nil
	}

	respType := reflect.TypeOf(resp)
	utils.DebugLog("Processing response of type: %v", respType)

	switch {
	case respType == nil:
		return resp
	case strings.Contains(respType.String(), "[]xtream"):
		return processXtreamArray(resp)
	case strings.Contains(respType.String(), "xtream"):
		return processXtreamStruct(resp)
	case strings.Contains(respType.String(), "VideoOnDemandInfo"):
		// Special handling for VideoOnDemandInfo which contains FFMPEGStreamInfo
		utils.DebugLog("Processing VideoOnDemandInfo specifically")
		return processXtreamStruct(resp)
	default:
		utils.DebugLog("No special processing for type: %v", respType)
	}
	return resp
}

func processXtreamArray(arr interface{}) interface{} {
	v := reflect.ValueOf(arr)
	if v.Kind() != reflect.Slice {
		return arr
	}
	if v.Len() == 0 {
		return arr
	}
	if !isXtreamCodesStruct(v.Index(0).Interface()) {
		return arr
	}
	result := make([]interface{}, v.Len())
	for i := 0; i < v.Len(); i++ {
		result[i] = processXtreamStruct(v.Index(i).Interface())
	}
	return result
}

func hasFieldsField(item interface{}) bool {
	respValue := reflect.ValueOf(item)
	if respValue.Kind() == reflect.Ptr {
		respValue = respValue.Elem()
	}
	if respValue.Kind() != reflect.Struct {
		return false
	}
	fieldValue := respValue.FieldByName("Fields")
	return fieldValue.IsValid() && fieldValue.CanInterface() && !fieldValue.IsZero()
}

func isXtreamCodesStruct(item interface{}) bool {
	if item == nil {
		return false
	}
	respType := reflect.TypeOf(item)
	if respType == nil {
		return false
	}
	typeStr := respType.String()
	isXtreamType := strings.Contains(typeStr, "xtreamcodes.") ||
		strings.Contains(typeStr, "xtreamapi.") ||
		strings.Contains(typeStr, "*xtream.") ||
		strings.Contains(typeStr, "FFMPEGStreamInfo") ||
		strings.Contains(typeStr, "VideoOnDemandInfo")
	if isXtreamType && hasFieldsField(item) {
		return true
	}
	if strings.Contains(typeStr, "VideoOnDemandInfo") {
		utils.DebugLog("Found VideoOnDemandInfo, special handling needed")
		return true
	}
	return false
}

func processXtreamStruct(item interface{}) interface{} {
	if item == nil {
		return nil
	}
	respType := reflect.TypeOf(item)
	if respType == nil {
		return item
	}
	utils.DebugLog("Processing struct of type: %v", respType)
	if strings.Contains(respType.String(), "VideoOnDemandInfo") {
		utils.DebugLog("Special handling for VideoOnDemandInfo")
		respValue := reflect.ValueOf(item)
		if respValue.Kind() == reflect.Ptr {
			respValue = respValue.Elem()
		}
		fieldValue := respValue.FieldByName("Fields")
		if fieldValue.IsValid() && fieldValue.CanInterface() && !fieldValue.IsZero() {
			if fieldValue.Kind() == reflect.Slice && fieldValue.Type().Elem().Kind() == reflect.Uint8 {
				var rawMap map[string]interface{}
				if err := json.Unmarshal(fieldValue.Interface().([]byte), &rawMap); err != nil {
					utils.DebugLog("Error unmarshaling VideoOnDemandInfo: %v", err)
					return item
				}
				return rawMap
			}
		}
	}
	if isXtreamCodesStruct(item) {
		respValue := reflect.ValueOf(item)
		if respValue.Kind() == reflect.Ptr {
			respValue = respValue.Elem()
		}
		fieldValue := respValue.FieldByName("Fields")
		if fieldValue.IsValid() && fieldValue.CanInterface() && !fieldValue.IsZero() {
			if fieldValue.Kind() == reflect.Slice && fieldValue.Type().Elem().Kind() == reflect.Uint8 {
				var unmarshaledValue interface{}
				if err := json.Unmarshal(fieldValue.Interface().([]byte), &unmarshaledValue); err != nil {
					utils.DebugLog("-- processXtreamStruct: JSON unmarshal error: %v", err)
					return item
				}
				return unmarshaledValue
			}
			return fieldValue.Interface()
		}
	}
	return item
}
