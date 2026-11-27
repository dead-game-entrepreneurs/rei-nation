package tsfp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
	"golang.org/x/text/cases"
)

const maxMapSize = 0x800a
const maxRecordID = 2147483647
const mapDateFormat = "01/02/2006"

var mapAttribute = regexp.MustCompile("^[A-Za-z0-9_-]{1,27}$")
var mapNameFold = cases.Fold()

func mapNameKey(name string) string {
	return mapNameFold.String(strings.Join(strings.Fields(name), " "))
}

func (f *fesl) mapTransaction(ctx context.Context, s *feslSession, tag, command string, v map[string]string) []string {
	if s.persona.ID == 0 {
		return failure("101")
	}
	if f.content == nil {
		return failure("99")
	}
	if tag == "fdbk" {
		if command != "sendfeedback" {
			return failure("21")
		}
		return f.feedback(ctx, s.persona, v)
	}
	if tag == "chnk" {
		if command == "getnextchunk" {
			return mapPage(s, 30)
		}
		s.maps = nil
		return []string{}
	}
	switch command {
	case "addblob":
		return f.upload(ctx, s.persona, v)
	case "listblobinfo", "topnblobdownloads", "topnblobratings":
		s.maps = nil
		filter, err := newMapFilter(command, v, time.Now().UTC())
		if err != nil {
			return failure("21")
		}
		order := filter.order
		if order == "listblobinfo" && filter.name != "" {
			order = "listblobinfo-name"
		}
		if filter.limit == 0 {
			return mapPage(s, 32)
		}
		err = f.content.TSFPMaps(ctx, order, func(m storage.TSFPMap) bool {
			if filter.matches(m) {
				s.maps = append(s.maps, m)
			}
			return len(s.maps) < filter.limit
		})
		if err != nil {
			return failure("99")
		}
		return mapPage(s, 32)
	}
	id, ok := decimal(v["BLOBID"], maxRecordID)
	if !ok || id == 0 {
		return failure("21")
	}
	var found bool
	var err error
	pairs := []string{}
	switch command {
	case "getblobcontent":
		var raw []byte
		raw, found, err = f.content.DownloadTSFPMap(ctx, int64(id))
		pairs = []string{"blobId", strconv.Itoa(id), "size", strconv.Itoa(base64.StdEncoding.EncodedLen(len(raw))),
			"content", base64.StdEncoding.EncodeToString(raw)}
	case "getblobinfo":
		var m storage.TSFPMap
		m, found, err = f.content.TSFPMap(ctx, int64(id))
		pairs = mapFields(m, "")
	case "removeblob":
		found, err = f.content.DeleteTSFPMap(ctx, s.persona.ID, int64(id))
	case "updateblobrating":
		rating, valid := decimal(v["RATING"], 5)
		if !valid {
			return failure("21")
		}
		found, err = f.content.RateTSFPMap(ctx, s.persona.ID, int64(id), rating)
	default:
		return failure("21")
	}
	if err != nil {
		return failure("99")
	}
	if !found {
		return failure("101")
	}
	return pairs
}

func (f *fesl) upload(ctx context.Context, p storage.Persona, v map[string]string) []string {
	owner, ok := decimal(v["OWNERID"], maxRecordID)
	size, sizeOK := decimal(v["SIZE"], maxMapSize)
	if !ok || int64(owner) != p.ID || !sizeOK || size == 0 || v["OWNERTYPE"] != "1" || v["TYPE"] != "2" ||
		len(v["CONTENT"]) > base64.StdEncoding.EncodedLen(maxMapSize) {
		return failure("21")
	}
	raw, err := base64.StdEncoding.DecodeString(v["CONTENT"])
	if err != nil || len(raw) != size {
		return failure("21")
	}
	meta := storage.TSFPMapMeta{
		Name: short(v["NAME"], 31), Creator: p.Name, Version: short(v["VERSION"], 31),
		ShortDescription: short(v["SHORTDESCRIPTION"], 79), LongDescription: short(v["LONGDESCRIPTION"], 254),
		Locale: short(v["LOCALE"], 31), FormatType: fallback(v, "FORMATTYPE", "0"), IconID: fallback(v, "ICONID", "0"),
	}
	nameKey := mapNameKey(meta.Name)
	attrs, valid := mapAttributes(v, "ATTRIBUTES")
	_, formatOK := decimal(meta.FormatType, maxRecordID)
	_, iconOK := decimal(meta.IconID, maxRecordID)
	if !valid || nameKey == "" || !formatOK || !iconOK || strings.ContainsRune(nameKey, 0) {
		return failure("21")
	}
	meta.Attributes = attrs
	id, err := f.content.CreateTSFPMap(ctx, p.ID, nameKey, meta, raw)
	if errors.Is(err, storage.ErrConflict) {
		return failure("7007")
	}
	if err != nil {
		return failure("99")
	}
	return []string{"blobId", strconv.FormatInt(id, 10)}
}

func mapAttributes(v map[string]string, root string) ([][2]string, bool) {
	n, ok := decimal(fallback(v, root+".[]", "0"), 32)
	if !ok {
		return nil, false
	}
	attrs := make([][2]string, n)
	for i := range n {
		prefix := root + "." + strconv.Itoa(i) + "."
		name, value := v[prefix+"NAME"], v[prefix+"VALUE"]
		if !mapAttribute.MatchString(name) || len(value) > 27 || strings.ContainsRune(value, 0) ||
			fallback(v, prefix+"TYPE", "0") != "0" {
			return nil, false
		}
		attrs[i] = [2]string{name, value}
	}
	return attrs, true
}

func mapFields(m storage.TSFPMap, prefix string) []string {
	meta := m.Meta
	date := m.Created.UTC().Format(mapDateFormat)
	pairs := []string{"blobId", strconv.FormatInt(m.ID, 10), "ownerId", strconv.FormatInt(m.Owner, 10),
		"ownerType", "1", "type", "2", "formatType", meta.FormatType, "iconId", meta.IconID, "createDate", date,
		"updateDate", date, "creator", meta.Creator, "name", meta.Name, "downloadCount", strconv.Itoa(m.Downloads),
		"rating", strconv.FormatFloat(m.Rating, 'f', 4, 64), "reviewCount", strconv.Itoa(m.Reviews), "version", meta.Version,
		"shortDescription", meta.ShortDescription, "longDescription", meta.LongDescription, "locale", meta.Locale,
		"attributes.[]", strconv.Itoa(len(meta.Attributes))}
	for i, attr := range meta.Attributes {
		key := "attributes." + strconv.Itoa(i) + "."
		pairs = append(pairs, key+"name", attr[0], key+"type", "0", key+"value", attr[1])
	}
	for i := 0; i < len(pairs); i += 2 {
		pairs[i] = prefix + pairs[i]
	}
	return pairs
}

func mapPage(s *feslSession, size int) []string {
	n := min(size, len(s.maps))
	pairs := []string{"nextChunkFlag", strconv.Itoa(boolInt(n < len(s.maps))), "blobs.[]", strconv.Itoa(n), "ownerType", "1"}
	for i, m := range s.maps[:n] {
		pairs = append(pairs, mapFields(m, "blobs."+strconv.Itoa(i)+".")...)
	}
	s.maps = s.maps[n:]
	if len(s.maps) == 0 {
		s.maps = nil
	}
	return pairs
}

type mapFilter struct {
	numbers       map[string]float64
	start, end    time.Time
	name          string
	caseSensitive bool
	wildcard      bool
	attrs         [][2]string
	limit         int
	order         string
}

func newMapFilter(command string, v map[string]string, now time.Time) (mapFilter, error) {
	f := mapFilter{numbers: make(map[string]float64), limit: 2000, order: command, name: v["NAME"],
		caseSensitive: v["NAMECASESENSITIVE"] == "1", wildcard: v["NAMEWILDCARDMATCH"] == "1"}
	if !f.caseSensitive {
		f.name = mapNameFold.String(f.name)
	}
	if f.wildcard {
		if _, err := path.Match(f.name, ""); err != nil {
			return f, err
		}
	}
	for _, key := range []string{"TYPE", "OWNERTYPE", "OWNERID", "FORMATTYPE", "ICONID", "RATINGMIN", "RATINGMAX",
		"DOWNLOADMIN", "DOWNLOADMAX", "SIZEMIN", "SIZEMAX", "MAXRECORDS", "TOPN", "PERIODTYPE", "PERIODSPAST"} {
		if v[key] == "" || command == "topnblobdownloads" && strings.HasPrefix(key, "RATING") {
			continue
		}
		n, err := strconv.ParseFloat(v[key], 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n > maxRecordID {
			return f, fmt.Errorf("invalid map filter value for %s", key)
		}
		if n >= 0 {
			f.numbers[key] = n
		}
	}
	if command != "listblobinfo" {
		day := now.Truncate(24 * time.Hour)
		past := min(int(f.numbers["PERIODSPAST"]), 3660)
		switch int(f.numbers["PERIODTYPE"]) {
		case 1:
			f.start = day.AddDate(0, 0, -past)
			f.end = f.start.AddDate(0, 0, 1)
		case 2:
			f.start = day.AddDate(0, 0, -(int(day.Weekday())+6)%7-7*past)
			f.end = f.start.AddDate(0, 0, 7)
		case 3:
			f.start = time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -past, 0)
			f.end = f.start.AddDate(0, 1, 0)
		}
	}
	for _, prefix := range []string{"CREATEDATE", "UPDATEDATE"} {
		if raw := v[prefix+"START"]; raw != "" {
			start, err := time.Parse(mapDateFormat, raw)
			if err != nil {
				return f, err
			}
			if f.start.Before(start) {
				f.start = start
			}
		}
		if raw := v[prefix+"END"]; raw != "" {
			end, err := time.Parse(mapDateFormat, raw)
			if err != nil {
				return f, err
			}
			end = end.AddDate(0, 0, 1)
			if f.end.IsZero() || end.Before(f.end) {
				f.end = end
			}
		}
	}
	for _, key := range []string{"MAXRECORDS", "TOPN"} {
		if n, ok := f.numbers[key]; ok {
			f.limit = min(f.limit, int(n))
		}
	}
	var ok bool
	f.attrs, ok = mapAttributes(v, "SEARCHATTRIBUTES")
	if !ok {
		return f, errors.New("invalid map filter attributes")
	}
	return f, nil
}

func (f mapFilter) matches(m storage.TSFPMap) bool {
	if (!f.start.IsZero() && m.Created.Before(f.start)) || (!f.end.IsZero() && !m.Created.Before(f.end)) {
		return false
	}
	format, _ := strconv.ParseFloat(m.Meta.FormatType, 64)
	icon, _ := strconv.ParseFloat(m.Meta.IconID, 64)
	for key, actual := range map[string]float64{"TYPE": 2, "OWNERTYPE": 1, "OWNERID": float64(m.Owner), "FORMATTYPE": format, "ICONID": icon} {
		if expected, ok := f.numbers[key]; ok && actual != expected {
			return false
		}
	}
	for key, actual := range map[string]float64{"RATING": m.Rating, "DOWNLOAD": float64(m.Downloads), "SIZE": float64(m.Size)} {
		if n, ok := f.numbers[key+"MIN"]; ok && actual < n {
			return false
		}
		if n, ok := f.numbers[key+"MAX"]; ok && actual > n {
			return false
		}
	}
	if f.name != "" {
		name := m.Meta.Name
		if !f.caseSensitive {
			name = mapNameFold.String(name)
		}
		match := name == f.name
		if f.wildcard {
			match, _ = path.Match(f.name, name)
		}
		if !match {
			return false
		}
	}
	for _, request := range f.attrs {
		var found bool
		for _, attr := range m.Meta.Attributes {
			if strings.EqualFold(attr[0], request[0]) && strings.EqualFold(attr[1], request[1]) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (f *fesl) feedback(ctx context.Context, p storage.Persona, v map[string]string) []string {
	reporter, reporterOK := decimal(v["ORIGINATORUSERID"], maxRecordID)
	kind, kindOK := decimal(v["TARGETTYPE"], 3)
	reason, reasonOK := decimal(v["FEEDBACKTYPE"], 1000)
	detail, message := v["EXTRAFEEDBACKDETAIL"], v["MESSAGE"]
	if !reporterOK || int64(reporter) != p.ID || !kindOK || !reasonOK || kind != 1 && kind != 3 ||
		len(detail) > 512 || len(message) > 1024 || strings.ContainsRune(detail+message, 0) {
		return failure("21")
	}
	var targetID int64
	if kind == 1 && v["TARGETNAMES.[]"] == "1" {
		target, ok := f.messenger.target(ctx, v["TARGETNAMES.0"])
		if ok {
			targetID = target.ID
		}
	} else if v["TARGETIDS.[]"] == "1" {
		id, valid := decimal(v["TARGETIDS.0"], maxRecordID)
		if !valid {
			return failure("21")
		}
		targetID = int64(id)
	}
	if targetID == 0 {
		return failure("21")
	}
	ok, err := f.content.RecordTSFPFeedback(ctx, storage.TSFPFeedback{
		Reporter: p.ID, Target: targetID, TargetType: kind, Reason: reason, Detail: detail, Message: message,
	})
	if err != nil {
		return failure("99")
	}
	if !ok {
		return failure("21")
	}
	return []string{}
}
