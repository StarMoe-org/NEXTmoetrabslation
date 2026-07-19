package translator

import "moesekai/server/internal/store"

const jpInformationURL = "https://baijing.exmeaning.com/jp/information"

// extractResult is the per-category output: field -> {pairs, trace}.
type extractResult map[string]store.CNApplyField

func newExtractResult(fields ...string) extractResult {
	r := make(extractResult, len(fields))
	for _, f := range fields {
		r[f] = store.CNApplyField{Pairs: map[string]string{}, Trace: map[string][]string{}}
	}
	return r
}

// toCNApply converts extractResult + traceMap into the store apply input.
func (r extractResult) withTrace(tm traceMap) map[string]store.CNApplyField {
	out := make(map[string]store.CNApplyField, len(r))
	for field, f := range r {
		f.Trace = tm[field]
		if f.Trace == nil {
			f.Trace = map[string][]string{}
		}
		out[field] = f
	}
	return out
}

func (t *Translator) extractCards() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("cards.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("cards.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("prefix", "skillName", "gachaPhrase")
	tm := newTraceMap("prefix", "skillName", "gachaPhrase")
	for _, item := range jp {
		id := getInt(item, "id")
		cnItem := cnByID[id]
		jpPrefix := getString(item, "prefix")
		tm.add("prefix", jpPrefix, id)
		collectPair(out["prefix"].Pairs, jpPrefix, getString(cnItem, "prefix"))
		jpSkill := getString(item, "cardSkillName")
		tm.add("skillName", jpSkill, id)
		collectPair(out["skillName"].Pairs, jpSkill, getString(cnItem, "cardSkillName"))
		phrase := getString(item, "gachaPhrase")
		if phrase != "" && phrase != "-" {
			tm.add("gachaPhrase", phrase, id)
			collectPair(out["gachaPhrase"].Pairs, phrase, getString(cnItem, "gachaPhrase"))
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractSkills() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("skills.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("skills.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("description")
	tm := newTraceMap("description")
	for _, item := range jp {
		id := getInt(item, "id")
		cnItem := cnByID[id]
		jpDesc := getString(item, "description")
		tm.add("description", jpDesc, id)
		collectPair(out["description"].Pairs, jpDesc, getString(cnItem, "description"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractInformation() (map[string]store.CNApplyField, error) {
	raw, err := t.fetchJSONURL(jpInformationURL)
	if err != nil {
		return nil, err
	}
	items := toMapSlice(asMap(raw)["informations"])
	out := newExtractResult("title")
	tm := newTraceMap("title")
	for _, item := range items {
		id := getInt(item, "id")
		title := safeText(getString(item, "title"))
		tm.add("title", title, id)
		collectPair(out["title"].Pairs, title, "")
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractSimpleNameByID(file, idField, nameField string) (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata(file, "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata(file, "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, idField)
	out := newExtractResult("name")
	tm := newTraceMap("name")
	for _, item := range jp {
		id := getInt(item, idField)
		jpName := getString(item, nameField)
		tm.add("name", jpName, id)
		collectPair(out["name"].Pairs, jpName, getString(cnByID[id], nameField))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractEvents() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("events.json", "id", "name")
}
func (t *Translator) extractGacha() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("gachas.json", "id", "name")
}
func (t *Translator) extractVirtualLive() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("virtualLives.json", "id", "name")
}
func (t *Translator) extractStickers() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("stamps.json", "id", "name")
}

func (t *Translator) extractComics() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("tips.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("tips.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("title")
	tm := newTraceMap("title")
	for _, item := range jp {
		if getString(item, "assetbundleName") == "" {
			continue
		}
		id := getInt(item, "id")
		jpTitle := getString(item, "title")
		tm.add("title", jpTitle, id)
		collectPair(out["title"].Pairs, jpTitle, getString(cnByID[id], "title"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractMusic() (map[string]store.CNApplyField, error) {
	musics, err := t.fetchMasterdata("musics.json", "jp")
	if err != nil {
		return nil, err
	}
	vocals, _ := t.fetchMasterdata("musicVocals.json", "jp")
	out := newExtractResult("title", "artist", "vocalCaption")
	tm := newTraceMap("title", "artist", "vocalCaption")
	for _, m := range musics {
		musicID := getInt(m, "id")
		if title := getString(m, "title"); title != "" {
			out["title"].Pairs[title] = ""
			tm.add("title", title, musicID)
		}
		for _, key := range []string{"lyricist", "composer", "arranger"} {
			if v := getString(m, key); v != "" && v != "-" {
				out["artist"].Pairs[v] = ""
				tm.add("artist", v, musicID)
			}
		}
	}
	for _, v := range vocals {
		vocalID := getInt(v, "id")
		if vocalID == 0 {
			vocalID = getInt(v, "musicId")
		}
		if caption := getString(v, "caption"); caption != "" {
			out["vocalCaption"].Pairs[caption] = ""
			tm.add("vocalCaption", caption, vocalID)
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractMysekai() (map[string]store.CNApplyField, error) {
	out := newExtractResult("fixtureName", "flavorText", "genre", "tag")
	tm := newTraceMap("fixtureName", "flavorText", "genre", "tag")

	jpFix, err := t.fetchMasterdata("mysekaiFixtures.json", "jp")
	if err != nil {
		return nil, err
	}
	cnFix, err := t.fetchMasterdata("mysekaiFixtures.json", "cn")
	if err != nil {
		return nil, err
	}
	cnFixByID := byIntID(cnFix, "id")
	for _, f := range jpFix {
		id := getInt(f, "id")
		cnf := cnFixByID[id]
		jpName := getString(f, "name")
		tm.add("fixtureName", jpName, id)
		collectPair(out["fixtureName"].Pairs, jpName, getString(cnf, "name"))
		jpFlavor := getString(f, "flavorText")
		tm.add("flavorText", jpFlavor, id)
		collectPair(out["flavorText"].Pairs, jpFlavor, getString(cnf, "flavorText"))
	}

	jpGenre, _ := t.fetchMasterdata("mysekaiFixtureMainGenres.json", "jp")
	cnGenre, err := t.fetchMasterdata("mysekaiFixtureMainGenres.json", "cn")
	if err != nil {
		return nil, err
	}
	cnGenreByID := byIntID(cnGenre, "id")
	for _, g := range jpGenre {
		id := getInt(g, "id")
		jpName := getString(g, "name")
		tm.add("genre", jpName, id)
		collectPair(out["genre"].Pairs, jpName, getString(cnGenreByID[id], "name"))
	}

	jpTag, _ := t.fetchMasterdata("mysekaiFixtureTags.json", "jp")
	cnTag, err := t.fetchMasterdata("mysekaiFixtureTags.json", "cn")
	if err != nil {
		return nil, err
	}
	cnTagByID := byIntID(cnTag, "id")
	for _, g := range jpTag {
		id := getInt(g, "id")
		jpName := getString(g, "name")
		tm.add("tag", jpName, id)
		collectPair(out["tag"].Pairs, jpName, getString(cnTagByID[id], "name"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractCostumes() (map[string]store.CNApplyField, error) {
	out := newExtractResult("name", "colorName", "designer")
	tm := newTraceMap("name", "colorName", "designer")
	jpRaw, err := t.fetchMasterdataDocument("snowy_costumes.json", "jp")
	if err != nil {
		return nil, err
	}
	cnRaw, err := t.fetchMasterdataDocument("snowy_costumes.json", "cn")
	if err != nil {
		return nil, err
	}
	jpList := toMapSlice(asMap(jpRaw)["costumes"])
	cnList := toMapSlice(asMap(cnRaw)["costumes"])
	cnByID := byIntID(cnList, "id")
	for _, costume := range jpList {
		id := getInt(costume, "id")
		cnCostume := cnByID[id]
		jpName := safeText(getString(costume, "name"))
		tm.add("name", jpName, id)
		collectPair(out["name"].Pairs, jpName, safeText(getString(cnCostume, "name")))
		jpDesigner := safeText(getString(costume, "designer"))
		tm.add("designer", jpDesigner, id)
		collectPair(out["designer"].Pairs, jpDesigner, safeText(getString(cnCostume, "designer")))

		jpParts := toParts(costume["parts"])
		cnParts := toParts(cnCostume["parts"])
		for partType, partList := range jpParts {
			cnPartByAsset := map[string]map[string]any{}
			for _, p := range cnParts[partType] {
				cnPartByAsset[getString(p, "assetbundleName")] = p
			}
			for _, p := range partList {
				jpColor := safeText(getString(p, "colorName"))
				if jpColor == "" {
					continue
				}
				tm.add("colorName", jpColor, id)
				cnColor := safeText(getString(cnPartByAsset[getString(p, "assetbundleName")], "colorName"))
				collectPair(out["colorName"].Pairs, jpColor, cnColor)
			}
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractCharacters() (map[string]store.CNApplyField, error) {
	fields := []string{"hobby", "specialSkill", "favoriteFood", "hatedFood", "weak", "introduction"}
	out := newExtractResult(fields...)
	tm := newTraceMap(fields...)
	jp, err := t.fetchMasterdata("characterProfiles.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("characterProfiles.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "characterId")
	for _, profile := range jp {
		id := getInt(profile, "characterId")
		cnProfile := cnByID[id]
		for _, field := range fields {
			jpText := safeText(getString(profile, field))
			tm.add(field, jpText, id)
			collectPair(out[field].Pairs, jpText, safeText(getString(cnProfile, field)))
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractUnits() (map[string]store.CNApplyField, error) {
	out := newExtractResult("unitName", "profileSentence")
	tm := newTraceMap("unitName", "profileSentence")
	jp, err := t.fetchMasterdata("unitProfiles.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("unitProfiles.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByUnit := map[string]map[string]any{}
	for _, unit := range cn {
		cnByUnit[getString(unit, "unit")] = unit
	}
	for _, unit := range jp {
		id := getString(unit, "unit")
		cnUnit := cnByUnit[id]
		jpUnitName := getString(unit, "unitName")
		tm.addStr("unitName", jpUnitName, id)
		collectPair(out["unitName"].Pairs, jpUnitName, getString(cnUnit, "unitName"))
		jpSentence := getString(unit, "profileSentence")
		tm.addStr("profileSentence", jpSentence, id)
		collectPair(out["profileSentence"].Pairs, jpSentence, getString(cnUnit, "profileSentence"))
	}
	return out.withTrace(tm), nil
}
