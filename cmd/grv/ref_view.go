package main

import (
	"fmt"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
)

type refViewHandler func(*RefView, Action) error

// RenderedRefType is the type (branch, tag, etc...) of a rendered ref
type RenderedRefType int

// The set of RenderedRefTypes
const (
	RvLocalBranchGroup RenderedRefType = iota
	RvRemoteBranchGroup
	RvLocalBranch
	RvRemoteBranch
	RvTagGroup
	RvTag
	RvSpace
	RvLoading
)

var refToTheme = map[RenderedRefType]ThemeComponentID{
	RvLocalBranchGroup:  CmpRefviewLocalBranchesHeader,
	RvRemoteBranchGroup: CmpRefviewRemoteBranchesHeader,
	RvLocalBranch:       CmpRefviewLocalBranch,
	RvRemoteBranch:      CmpRefviewRemoteBranch,
	RvTagGroup:          CmpRefviewTagsHeader,
	RvTag:               CmpRefviewTag,
}

type renderedRefGenerator func(*RefView, *refList, renderedRefSet)

type refList struct {
	name            string
	expanded        bool
	renderer        renderedRefGenerator
	renderedRefType RenderedRefType
}

// RenderedRef represents a reference's string value and meta data
type RenderedRef struct {
	value           string
	oid             *Oid
	renderedRefType RenderedRefType
	refList         *refList
	refNum          uint
}

type renderedRefSet interface {
	Add(*RenderedRef)
	AddChild(renderedRefSet)
	RemoveChild() (removed bool)
	Child() renderedRefSet
	Clear()
	RenderedRefs() []*RenderedRef
	Children() uint
}

type renderedRefList struct {
	child        renderedRefSet
	renderedRefs []*RenderedRef
	refFilter    *RefFilter
}

func newRenderedRefList() *renderedRefList {
	return newFilteredRenderedRefList(nil)
}

func newFilteredRenderedRefList(refFilter *RefFilter) *renderedRefList {
	return &renderedRefList{
		refFilter: refFilter,
	}
}

// Add a ref to the list if it matches the filter (if set) and pass it down the child filter
func (renderedRefList *renderedRefList) Add(renderedRef *RenderedRef) {
	if renderedRefList.refFilter != nil && !renderedRefList.refFilter.MatchesFilter(renderedRef) {
		return
	}

	renderedRefList.renderedRefs = append(renderedRefList.renderedRefs, renderedRef)

	if renderedRefList.child != nil {
		renderedRefList.child.Add(renderedRef)
	}
}

// AddChild adds another ref set and initialises it with its parents references
func (renderedRefList *renderedRefList) AddChild(renderedRefs renderedRefSet) {
	if renderedRefList.child != nil {
		renderedRefList.child.AddChild(renderedRefs)
	} else {
		renderedRefList.child = renderedRefs

		for _, renderedRef := range renderedRefList.renderedRefs {
			renderedRefs.Add(renderedRef)
		}
	}
}

// Remove child removes the last child in the chain
func (renderedRefList *renderedRefList) RemoveChild() (removed bool) {
	switch {
	case renderedRefList.Child() == nil:
	case renderedRefList.Child().Child() == nil:
		renderedRefList.child = nil
		removed = true
	default:
		removed = renderedRefList.Child().RemoveChild()
	}

	return
}

// Child returns the child
func (renderedRefList *renderedRefList) Child() renderedRefSet {
	return renderedRefList.child
}

// Clear clears the list of rendered refs for this instance and all its children
func (renderedRefList *renderedRefList) Clear() {
	renderedRefList.renderedRefs = renderedRefList.renderedRefs[0:0]

	if renderedRefList.child != nil {
		renderedRefList.child.Clear()
	}
}

// RenderedRefs returns the leaf childs set of rendered refs
func (renderedRefList *renderedRefList) RenderedRefs() []*RenderedRef {
	if renderedRefList.child != nil {
		return renderedRefList.child.RenderedRefs()
	}

	return renderedRefList.renderedRefs
}

// Children returns a count of the number of children this instance has
func (renderedRefList *renderedRefList) Children() (children uint) {
	renderedRefs := renderedRefList.Child()

	for ; renderedRefs != nil; renderedRefs = renderedRefs.Child() {
		children++
	}

	return
}

// RefView manages the display of references
type RefView struct {
	channels      *Channels
	repoData      RepoData
	refLists      []*refList
	refListeners  []RefListener
	active        bool
	renderedRefs  renderedRefSet
	viewPos       ViewPos
	viewDimension ViewDimension
	handlers      map[ActionType]refViewHandler
	viewSearch    *ViewSearch
	lock          sync.Mutex
}

// RefListener is notified when a reference is selected
type RefListener interface {
	OnRefSelect(refName string, oid *Oid) error
}

// NewRefView creates a new instance
func NewRefView(repoData RepoData, channels *Channels) *RefView {
	refView := &RefView{
		channels:     channels,
		repoData:     repoData,
		viewPos:      NewViewPosition(),
		renderedRefs: newRenderedRefList(),
		refLists: []*refList{
			{
				name:            "Branches",
				renderer:        generateBranches,
				expanded:        true,
				renderedRefType: RvLocalBranchGroup,
			},
			{
				name:            "Remote Branches",
				renderer:        generateBranches,
				renderedRefType: RvRemoteBranchGroup,
			},
			{
				name:            "Tags",
				renderer:        generateTags,
				renderedRefType: RvTagGroup,
			},
		},
		handlers: map[ActionType]refViewHandler{
			ActionPrevLine:     moveUpRef,
			ActionNextLine:     moveDownRef,
			ActionPrevPage:     moveUpRefPage,
			ActionNextPage:     moveDownRefPage,
			ActionScrollRight:  scrollRefViewRight,
			ActionScrollLeft:   scrollRefViewLeft,
			ActionFirstLine:    moveToFirstRef,
			ActionLastLine:     moveToLastRef,
			ActionSelect:       selectRef,
			ActionAddFilter:    addRefFilter,
			ActionRemoveFilter: removeRefFilter,
		},
	}

	refView.viewSearch = NewViewSearch(refView, channels)

	return refView
}

// Initialise loads the HEAD reference along with branches and tags
func (refView *RefView) Initialise() (err error) {
	log.Info("Initialising RefView")

	if err = refView.repoData.LoadHead(); err != nil {
		return
	}

	if err = refView.repoData.LoadBranches(func(localBranches, remoteBranches []*Branch) error {
		log.Debug("Branches loaded")
		refView.lock.Lock()
		defer refView.lock.Unlock()

		refView.generateRenderedRefs()

		_, headBranch := refView.repoData.Head()
		activeRowIndex := uint(1)

		if headBranch != nil {
			for _, branch := range localBranches {
				if branch.name == headBranch.name {
					log.Debugf("Setting branch %v as selected branch", branch.name)
					break
				}

				activeRowIndex++
			}
		}

		refView.viewPos.SetActiveRowIndex(activeRowIndex)
		refView.channels.UpdateDisplay()

		return nil
	}); err != nil {
		return
	}

	if err = refView.repoData.LoadLocalTags(func(tags []*Tag) error {
		log.Debug("Local tags loaded")
		refView.lock.Lock()
		defer refView.lock.Unlock()

		refView.generateRenderedRefs()
		refView.channels.UpdateDisplay()

		return nil
	}); err != nil {
		return
	}

	refView.generateRenderedRefs()
	head, branch := refView.repoData.Head()

	var branchName string
	if branch == nil {
		branchName = getDetachedHeadDisplayValue(head)
	} else {
		branchName = branch.name
	}

	err = refView.notifyRefListeners(branchName, head)

	return
}

func getDetachedHeadDisplayValue(oid *Oid) string {
	return fmt.Sprintf("HEAD detached at %s", oid.String()[0:7])
}

func isSelectableRenderedRef(renderedRefType RenderedRefType) bool {
	return renderedRefType != RvSpace && renderedRefType != RvLoading
}

// RegisterRefListener adds a ref listener to be notified when a reference is selected
func (refView *RefView) RegisterRefListener(refListener RefListener) {
	refView.refListeners = append(refView.refListeners, refListener)
}

func (refView *RefView) notifyRefListeners(refName string, oid *Oid) (err error) {
	log.Debugf("Notifying RefListeners of selected oid %v", oid)

	for _, refListener := range refView.refListeners {
		if err = refListener.OnRefSelect(refName, oid); err != nil {
			break
		}
	}

	return
}

// Render generates and writes the ref view to the provided window
func (refView *RefView) Render(win RenderWindow) (err error) {
	log.Debug("Rendering RefView")
	refView.lock.Lock()
	defer refView.lock.Unlock()

	refView.viewDimension = win.ViewDimensions()

	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))
	rows := win.Rows() - 2
	viewPos := refView.viewPos
	viewPos.DetermineViewStartRow(rows, renderedRefNum)
	refIndex := viewPos.ViewStartRowIndex()
	startColumn := viewPos.ViewStartColumn()

	for winRowIndex := uint(0); winRowIndex < rows && refIndex < renderedRefNum; winRowIndex++ {
		renderedRef := renderedRefs[refIndex]

		themeComponentID, ok := refToTheme[renderedRef.renderedRefType]
		if !ok {
			themeComponentID = CmpNone
		}

		if err = win.SetRow(winRowIndex+1, startColumn, themeComponentID, "%v", renderedRef.value); err != nil {
			return
		}

		refIndex++
	}

	if err = win.SetSelectedRow(viewPos.SelectedRowIndex()+1, refView.active); err != nil {
		return
	}

	win.DrawBorder()

	if err = win.SetTitle(CmpRefviewTitle, "Refs"); err != nil {
		return
	}

	selectedRenderedRef := renderedRefs[viewPos.ActiveRowIndex()]
	if err = refView.renderFooter(win, selectedRenderedRef); err != nil {
		return
	}

	if searchActive, searchPattern, lastSearchFoundMatch := refView.viewSearch.SearchActive(); searchActive && lastSearchFoundMatch {
		if err = win.Highlight(searchPattern, CmpAllviewSearchMatch); err != nil {
			return
		}
	}

	return
}

// RenderStatusBar does nothing
func (refView *RefView) RenderStatusBar(lineBuilder *LineBuilder) (err error) {
	return
}

// RenderHelpBar generates key binding help info for the ref view
func (refView *RefView) RenderHelpBar(lineBuilder *LineBuilder) (err error) {
	RenderKeyBindingHelp(refView.ViewID(), lineBuilder, []ActionMessage{
		{action: ActionSelect, message: "Select"},
		{action: ActionFilterPrompt, message: "Add Filter"},
		{action: ActionRemoveFilter, message: "Remove Filter"},
	})

	return
}

func (refView *RefView) renderFooter(win RenderWindow, selectedRenderedRef *RenderedRef) (err error) {
	var footer string

	if filters := refView.renderedRefs.Children(); filters > 0 {
		plural := ""
		if filters > 1 {
			plural = "s"
		}

		footer = fmt.Sprintf("%v filter%v applied", filters, plural)
	} else {
		switch selectedRenderedRef.renderedRefType {
		case RvLocalBranchGroup:
			if localBranches, _, loading := refView.repoData.Branches(); loading {
				footer = "Branches: Loading..."
			} else {
				footer = fmt.Sprintf("Branches: %v", len(localBranches))
			}
		case RvRemoteBranchGroup:
			if _, remoteBranches, loading := refView.repoData.Branches(); loading {
				footer = "Remote Branches: Loading..."
			} else {
				footer = fmt.Sprintf("Remote Branches: %v", len(remoteBranches))
			}
		case RvLocalBranch:
			localBranches, _, _ := refView.repoData.Branches()
			footer = fmt.Sprintf("Branch %v of %v", selectedRenderedRef.refNum, len(localBranches))
		case RvRemoteBranch:
			_, remoteBranches, _ := refView.repoData.Branches()
			footer = fmt.Sprintf("Remote Branch %v of %v", selectedRenderedRef.refNum, len(remoteBranches))
		case RvTagGroup:
			if tags, loading := refView.repoData.LocalTags(); loading {
				footer = "Tags: Loading"
			} else {
				footer = fmt.Sprintf("Tags: %v", len(tags))
			}
		case RvTag:
			tags, _ := refView.repoData.LocalTags()
			footer = fmt.Sprintf("Tag %v of %v", selectedRenderedRef.refNum, len(tags))
		}
	}

	if footer != "" {
		err = win.SetFooter(CmpRefviewFooter, "%v", footer)
	}

	return
}

func (refView *RefView) generateRenderedRefs() {
	log.Debug("Generating Rendered Refs")
	refView.renderedRefs.Clear()
	renderedRefs := refView.renderedRefs

	for refIndex, refList := range refView.refLists {
		expandChar := "+"
		if refList.expanded {
			expandChar = "-"
		}

		renderedRefs.Add(&RenderedRef{
			value:           fmt.Sprintf("  [%v] %v", expandChar, refList.name),
			refList:         refList,
			renderedRefType: refList.renderedRefType,
		})

		if refList.expanded {
			refList.renderer(refView, refList, renderedRefs)
		}

		if refIndex != len(refView.refLists)-1 {
			renderedRefs.Add(&RenderedRef{
				value:           "",
				renderedRefType: RvSpace,
			})
		}
	}
}

func generateBranches(refView *RefView, refList *refList, renderedRefs renderedRefSet) {
	localBranches, remoteBranches, loading := refView.repoData.Branches()

	if loading {
		renderedRefs.Add(&RenderedRef{
			value:           "   Loading...",
			renderedRefType: RvLoading,
		})

		return
	}

	branchNum := uint(1)
	var branches []*Branch
	var branchRenderedRefType RenderedRefType

	if refList.renderedRefType == RvLocalBranchGroup {
		branchRenderedRefType = RvLocalBranch
		branches = localBranches

		if head, headBranch := refView.repoData.Head(); headBranch == nil {
			renderedRefs.Add(&RenderedRef{
				value:           fmt.Sprintf("   %s", getDetachedHeadDisplayValue(head)),
				oid:             head,
				renderedRefType: branchRenderedRefType,
				refNum:          branchNum,
			})

			branchNum++
		}
	} else {
		branchRenderedRefType = RvRemoteBranch
		branches = remoteBranches
	}

	for _, branch := range branches {
		renderedRefs.Add(&RenderedRef{
			value:           fmt.Sprintf("   %s", branch.name),
			oid:             branch.oid,
			renderedRefType: branchRenderedRefType,
			refNum:          branchNum,
		})

		branchNum++
	}
}

func generateTags(refView *RefView, refList *refList, renderedRefs renderedRefSet) {
	tags, loading := refView.repoData.LocalTags()

	if loading {
		renderedRefs.Add(&RenderedRef{
			value:           "   Loading...",
			renderedRefType: RvLoading,
		})

		return
	}

	for tagIndex, tag := range tags {
		renderedRefs.Add(&RenderedRef{
			value:           fmt.Sprintf("   %s", tag.name),
			oid:             tag.oid,
			renderedRefType: RvTag,
			refNum:          uint(tagIndex + 1),
		})
	}
}

// OnActiveChange updates whether the ref view is active or not
func (refView *RefView) OnActiveChange(active bool) {
	log.Debugf("RefView active: %v", active)
	refView.lock.Lock()
	defer refView.lock.Unlock()

	refView.active = active
}

// ViewID returns the view ID of the ref view
func (refView *RefView) ViewID() ViewID {
	return ViewRef
}

// ViewPos returns the current cursor position in the view
func (refView *RefView) ViewPos() ViewPos {
	return refView.viewPos
}

// OnSearchMatch updates the view position to the matched search position
func (refView *RefView) OnSearchMatch(startPos ViewPos, matchLineIndex uint) {
	refView.lock.Lock()
	defer refView.lock.Unlock()

	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRef := renderedRefs[matchLineIndex]

	if isSelectableRenderedRef(renderedRef.renderedRefType) {
		refView.viewPos.SetActiveRowIndex(matchLineIndex)
	} else {
		log.Debugf("Unable to select search match at index %v as it is not a selectable type", matchLineIndex)
	}
}

// Line returns the rendered line specified by the provided line index
func (refView *RefView) Line(lineIndex uint) (line string) {
	refView.lock.Lock()
	defer refView.lock.Unlock()

	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))

	if lineIndex >= renderedRefNum {
		log.Errorf("Invalid lineIndex: %v", lineIndex)
		return
	}

	renderedRef := renderedRefs[lineIndex]
	line = renderedRef.value

	return
}

// LineNumber returns the number of lines in the ref view
func (refView *RefView) LineNumber() (lineNumber uint) {
	refView.lock.Lock()
	defer refView.lock.Unlock()

	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))
	return renderedRefNum
}

// HandleKeyPress does nothing
func (refView *RefView) HandleKeyPress(keystring string) (err error) {
	log.Debugf("RefView handling key %v - NOP", keystring)
	return
}

// HandleAction checks if the rev view supports an action and executes it if so
func (refView *RefView) HandleAction(action Action) (err error) {
	log.Debugf("RefView handling action %v", action)
	refView.lock.Lock()
	defer refView.lock.Unlock()

	if handler, ok := refView.handlers[action.ActionType]; ok {
		err = handler(refView, action)
	} else {
		_, err = refView.viewSearch.HandleAction(action)
	}

	return
}

func moveUpRef(refView *RefView, action Action) (err error) {
	viewPos := refView.viewPos

	if viewPos.ActiveRowIndex() == 0 {
		return
	}

	log.Debug("Moving up one ref")

	renderedRefs := refView.renderedRefs.RenderedRefs()
	startIndex := viewPos.ActiveRowIndex()
	activeRowIndex := startIndex - 1

	for activeRowIndex > 0 {
		renderedRef := renderedRefs[activeRowIndex]

		if isSelectableRenderedRef(renderedRef.renderedRefType) {
			break
		}

		activeRowIndex--
	}

	renderedRef := renderedRefs[activeRowIndex]
	if isSelectableRenderedRef(renderedRef.renderedRefType) {
		viewPos.SetActiveRowIndex(activeRowIndex)
		refView.channels.UpdateDisplay()
	} else {
		log.Debug("No valid ref entry to move to")
	}

	return
}

func moveDownRef(refView *RefView, action Action) (err error) {
	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))
	viewPos := refView.viewPos

	if renderedRefNum == 0 || !(viewPos.ActiveRowIndex() < renderedRefNum-1) {
		return
	}

	log.Debug("Moving down one ref")

	startIndex := viewPos.ActiveRowIndex()
	activeRowIndex := startIndex + 1

	for activeRowIndex < renderedRefNum-1 {
		renderedRef := renderedRefs[activeRowIndex]

		if isSelectableRenderedRef(renderedRef.renderedRefType) {
			break
		}

		activeRowIndex++
	}

	renderedRef := renderedRefs[activeRowIndex]
	if isSelectableRenderedRef(renderedRef.renderedRefType) {
		viewPos.SetActiveRowIndex(activeRowIndex)
		refView.channels.UpdateDisplay()
	} else {
		log.Debug("No valid ref entry to move to")
	}

	return
}

func moveUpRefPage(refView *RefView, action Action) (err error) {
	pageSize := refView.viewDimension.rows - 2
	viewPos := refView.viewPos

	for viewPos.ActiveRowIndex() > 0 && pageSize > 0 {
		if err = moveUpRef(refView, action); err != nil {
			break
		} else {
			pageSize--
		}
	}

	return
}

func moveDownRefPage(refView *RefView, action Action) (err error) {
	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))
	pageSize := refView.viewDimension.rows - 2
	viewPos := refView.viewPos

	for viewPos.ActiveRowIndex()+1 < renderedRefNum && pageSize > 0 {
		if err = moveDownRef(refView, action); err != nil {
			break
		} else {
			pageSize--
		}
	}

	return
}

func scrollRefViewRight(refView *RefView, action Action) (err error) {
	viewPos := refView.viewPos
	viewPos.MovePageRight(refView.viewDimension.cols)
	log.Debugf("Scrolling right. View starts at column %v", viewPos.ViewStartColumn())
	refView.channels.UpdateDisplay()

	return
}

func scrollRefViewLeft(refView *RefView, action Action) (err error) {
	viewPos := refView.viewPos

	if viewPos.MovePageLeft(refView.viewDimension.cols) {
		log.Debugf("Scrolling left. View starts at column %v", viewPos.ViewStartColumn())
		refView.channels.UpdateDisplay()
	}

	return
}

func moveToFirstRef(refView *RefView, action Action) (err error) {
	viewPos := refView.viewPos

	if viewPos.MoveToFirstLine() {
		log.Debugf("Moving to first ref")
		refView.channels.UpdateDisplay()
	}

	return
}

func moveToLastRef(refView *RefView, action Action) (err error) {
	viewPos := refView.viewPos
	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRefNum := uint(len(renderedRefs))

	if viewPos.MoveToLastLine(renderedRefNum) {
		log.Debugf("Moving to last ref")
		refView.channels.UpdateDisplay()
	}

	return
}

func selectRef(refView *RefView, action Action) (err error) {
	renderedRefs := refView.renderedRefs.RenderedRefs()
	renderedRef := renderedRefs[refView.viewPos.ActiveRowIndex()]

	switch renderedRef.renderedRefType {
	case RvLocalBranchGroup, RvRemoteBranchGroup, RvTagGroup:
		renderedRef.refList.expanded = !renderedRef.refList.expanded
		log.Debugf("Setting ref group %v to expanded %v", renderedRef.refList.name, renderedRef.refList.expanded)
		refView.generateRenderedRefs()
		refView.channels.UpdateDisplay()
	case RvLocalBranch, RvRemoteBranch, RvTag:
		log.Debugf("Selecting ref %v:%v", renderedRef.value, renderedRef.oid)
		if err = refView.notifyRefListeners(strings.TrimLeft(renderedRef.value, " "), renderedRef.oid); err != nil {
			return
		}
		refView.channels.UpdateDisplay()
	default:
		log.Warn("Unexpected ref type %v", renderedRef.renderedRefType)
	}

	return
}

func addRefFilter(refView *RefView, action Action) (err error) {
	if !(len(action.Args) > 0) {
		return fmt.Errorf("Expected filter query argument")
	}

	query, ok := action.Args[0].(string)
	if !ok {
		return fmt.Errorf("Expected filter query argument to have type string")
	}

	refFilter, errors := CreateRefFilter(query)
	if len(errors) > 0 {
		refView.channels.ReportErrors(errors)
		return
	}

	beforeRenderedRefNum := len(refView.renderedRefs.RenderedRefs())
	refView.renderedRefs.AddChild(newFilteredRenderedRefList(refFilter))
	afterRenderedRefNum := len(refView.renderedRefs.RenderedRefs())

	if afterRenderedRefNum < beforeRenderedRefNum {
		refView.channels.ReportStatus("Filter applied")
	} else {
		refView.channels.ReportStatus("Filter had no effect")
	}

	return
}

func removeRefFilter(refView *RefView, action Action) (err error) {
	if refView.renderedRefs.RemoveChild() {
		refView.channels.ReportStatus("Removed ref filter")
	} else {
		refView.channels.ReportStatus("No ref filter applied to remove")
	}

	return
}
