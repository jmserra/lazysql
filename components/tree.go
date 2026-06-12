package components

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/rivo/tview"

	"github.com/jorgerojas26/lazysql/app"
	"github.com/jorgerojas26/lazysql/commands"
	"github.com/jorgerojas26/lazysql/drivers"
	"github.com/jorgerojas26/lazysql/helpers/logger"
	"github.com/jorgerojas26/lazysql/models"
)

type TreeState struct {
	currentFocusFoundNode *tview.TreeNode
	selectedDatabase      string
	selectedTable         string
	searchFoundNodes      []*tview.TreeNode
	flatNodes             []*tview.TreeNode
	isFiltering           bool
}

type Tree struct {
	DBDriver drivers.Driver
	*tview.TreeView
	state               *TreeState
	Filter              *tview.InputField
	Wrapper             *tview.Flex
	FoundNodeCountInput *tview.InputField
	subscribers         []chan models.StateChange
	Schemas             []string
	flat                bool

	// loadErrors records, per database, the error hit while loading its tables
	// in the background so it can be surfaced when the user selects that
	// database instead of leaving the node silently empty. Guarded by its mutex
	// because the loader goroutines write while the event loop reads.
	loadErrors   map[string]string
	loadErrorsMu sync.Mutex
}

type TreeNodeType int

const (
	NodeTypeSection TreeNodeType = iota
	NodeTypeDatabase
	NodeTypeTable
	NodeTypeFunction
	NodeTypeProcedure
	NodeTypeView
)

type TreeNodeData struct {
	Type     TreeNodeType
	Database string
	Schema   string
	Name     string
}

func (tree *Tree) GetTreeNodeData(node *tview.TreeNode) *TreeNodeData {
	key := node.GetReference().(string)
	supportsProgramming := tree.DBDriver.SupportsProgramming()
	useSchemas := tree.DBDriver.UseSchemas()
	var nodeType TreeNodeType
	schema := ""

	split := strings.Split(key, ".")
	database := split[0]
	name := split[len(split)-1]

	switch {
	case len(split) == 1:
		nodeType = NodeTypeDatabase
	case len(split) == 2 && !useSchemas && !supportsProgramming:
		nodeType = NodeTypeTable
	case len(split) == 3 && useSchemas && !supportsProgramming:
		nodeType = NodeTypeTable
		schema = split[len(split)-2]
	case len(split) == 3 && !useSchemas && supportsProgramming:
		switch parentType := split[len(split)-2]; parentType {
		case "tables":
			nodeType = NodeTypeTable
		case "procedures":
			nodeType = NodeTypeProcedure
		case "functions":
			nodeType = NodeTypeFunction
		case "views":
			nodeType = NodeTypeView
		default:
			nodeType = NodeTypeSection
		}
	case len(split) == 4 && useSchemas && supportsProgramming:
		switch parentType := split[len(split)-2]; parentType {
		case "tables":
			nodeType = NodeTypeTable
		case "procedures":
			nodeType = NodeTypeProcedure
		case "functions":
			nodeType = NodeTypeFunction
		case "views":
			nodeType = NodeTypeView
		default:
			nodeType = NodeTypeSection
		}

		schema = split[len(split)-2]
	default:
		nodeType = NodeTypeSection
	}

	return &TreeNodeData{
		Type:     nodeType,
		Database: database,
		Schema:   schema,
		Name:     name,
	}
}

func NewTree(dbName string, dbdriver drivers.Driver, schemas []string) *Tree {
	state := &TreeState{
		selectedDatabase: "",
		selectedTable:    "",
	}

	tree := &Tree{
		Wrapper:             tview.NewFlex(),
		TreeView:            tview.NewTreeView(),
		state:               state,
		subscribers:         []chan models.StateChange{},
		DBDriver:            dbdriver,
		Filter:              tview.NewInputField(),
		FoundNodeCountInput: tview.NewInputField(),
		Schemas:             schemas,
		loadErrors:          make(map[string]string),
	}

	tree.SetTopLevel(1)
	tree.SetGraphicsColor(app.Styles.PrimaryTextColor)
	// tree.SetBorder(true)
	tree.SetTitleAlign(tview.AlignLeft)
	// tree.SetBorderPadding(0, 0, 1, 1)

	// Flat mode: a single database is targeted, so drop the tree graphics
	// (no |- connectors) and surface the database/schema as the pane header.
	if dbName != "" {
		header := dbName
		if len(schemas) == 1 {
			header = fmt.Sprintf("%s.%s", dbName, schemas[0])
		}
		tree.flat = true
		tree.SetGraphics(false)
		tree.Wrapper.SetTitle(header)
	} else {
		tree.Wrapper.SetTitle("Databases")
	}

	rootNode := tview.NewTreeNode("-")
	tree.SetRoot(rootNode)
	tree.SetCurrentNode(rootNode)

	tree.SetFocusFunc(func() {
		tree.InitializeNodes(dbName)
		tree.SetFocusFunc(nil)

		// In flat mode the table list is the whole tree, so drop the user
		// straight into search-table mode when the database opens.
		if dbName != "" {
			tree.RemoveHighlight()
			App.SetFocus(tree.Filter)
			tree.SetIsFiltering(true)
		}
	})

	selectedNodeTextColor := fmt.Sprintf("[black:%s]", app.Styles.SecondaryTextColor.Name())
	previouslyFocusedNode := tree.GetCurrentNode()
	previouslyFocusedNode.SetText(selectedNodeTextColor + previouslyFocusedNode.GetText())

	tree.SetChangedFunc(func(node *tview.TreeNode) {
		// Set colors on focused node
		nodeText := node.GetText()
		if !strings.Contains(nodeText, selectedNodeTextColor) {
			node.SetText(selectedNodeTextColor + nodeText)
		}

		// Remove colors on previously focused node
		previousNodeText := previouslyFocusedNode.GetText()
		splitNodeText := strings.Split(previousNodeText, selectedNodeTextColor)
		if len(splitNodeText) > 1 {
			previouslyFocusedNode.SetText(splitNodeText[1])
		}
		previouslyFocusedNode = node
	})

	tree.SetSelectedFunc(tree.selectNode)

	tree.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		command := app.Keymaps.Group(app.TreeGroup).Resolve(event)

		switch command {
		case commands.GotoBottom:
			childrens := tree.GetRoot().GetChildren()
			lastNode := childrens[len(childrens)-1]

			if lastNode.IsExpanded() {
				childNodes := lastNode.GetChildren()
				lastChildren := childNodes[len(childNodes)-1]
				tree.SetCurrentNode(lastChildren)
			} else {
				tree.SetCurrentNode(lastNode)
			}
		case commands.GotoTop:
			tree.SetCurrentNode(rootNode)
		case commands.PageNext:
			tree.Move(5)
		case commands.PagePrev:
			tree.Move(-5)
		case commands.MoveDown:
			tree.Move(1)
		case commands.MoveUp:
			tree.Move(-1)
		case commands.Execute:
			// Can't "select" the current node via TreeView api.
			// So fake it by sending it a Enter key event
			return tcell.NewEventKey(tcell.KeyEnter, 0, 0)
		case commands.Search:
			tree.RemoveHighlight()
			App.SetFocus(tree.Filter)
			tree.SetIsFiltering(true)
		case commands.NextFoundNode:
			tree.goToNextFoundNode()
		case commands.PreviousFoundNode:
			tree.goToPreviousFoundNode()
		case commands.TreeCollapseAll:
			tree.CollapseAll()
		case commands.ExpandAll:
			tree.ExpandAll()
		case commands.Refresh:
			tree.Refresh(dbName)
		}
		return nil
	})

	tree.Filter.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:

			filterText := tree.Filter.GetText()

			if filterText == "" {
				tree.ClearSearch()
			} else if len(tree.state.searchFoundNodes) > 0 {
				// Open the best match directly so the user doesn't have to press
				// Enter a second time on the tree. Opening the table moves focus
				// to it (via showTable), so leave filtering mode without grabbing
				// focus back to the tree.
				tree.SetIsFiltering(false)
				tree.selectNode(tree.GetCurrentNode())
				return
			}

		case tcell.KeyEscape:
			tree.ClearSearch()
		}

		tree.SetIsFiltering(false)
		tree.Highlight()
		App.SetFocus(tree)
	})

	tree.Filter.SetChangedFunc(func(text string) {
		// search() runs off the main goroutine: it calls App.Draw() (which is a
		// queued update) and would deadlock the event loop if invoked from this
		// input handler, which already runs on the main goroutine.
		go tree.search(text)
	})

	tree.Filter.SetFieldStyle(tcell.StyleDefault.Background(app.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor))
	tree.Filter.SetPlaceholderStyle(tcell.StyleDefault.Background(app.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.InverseTextColor))
	tree.Filter.SetBorderPadding(0, 0, 0, 0)
	tree.Filter.SetBorderColor(app.Styles.PrimaryTextColor)
	if tree.flat {
		tree.Filter.SetLabel("Filter: ")
	} else {
		tree.Filter.SetLabel("Search: ")
	}
	tree.Filter.SetLabelColor(app.Styles.InverseTextColor)

	tree.Filter.SetFocusFunc(func() {
		tree.Filter.SetLabelColor(app.Styles.TertiaryTextColor)
		tree.Filter.SetFieldTextColor(app.Styles.PrimaryTextColor)
	})

	tree.Filter.SetBlurFunc(func() {
		if tree.Filter.GetText() == "" {
			tree.Filter.SetLabelColor(app.Styles.InverseTextColor)
		} else {
			tree.Filter.SetLabelColor(app.Styles.TertiaryTextColor)
		}
		tree.Filter.SetFieldTextColor(app.Styles.InverseTextColor)
	})

	tree.FoundNodeCountInput.SetFieldStyle(tcell.StyleDefault.Background(app.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor))

	tree.Wrapper.SetDirection(tview.FlexRow)
	tree.Wrapper.SetBorder(false)
	tree.Wrapper.SetBorderPadding(0, 0, 0, 0)
	tree.Wrapper.SetTitleColor(app.Styles.PrimaryTextColor)

	tree.Wrapper.AddItem(tree.Filter, 1, 0, false)
	tree.Wrapper.AddItem(tree, 0, 1, true)

	return tree
}

// selectNode performs the action for a tree node: expanding sections/databases
// or publishing the selected table/procedure/function/view so it opens.
func (tree *Tree) selectNode(node *tview.TreeNode) {
	if node == nil {
		return
	}

	nodeData := tree.GetTreeNodeData(node)

	switch nodeData.Type {
	case NodeTypeSection:
		node.SetExpanded(!node.IsExpanded())
	case NodeTypeDatabase:
		if node.IsExpanded() {
			node.SetExpanded(false)
		} else {
			// A database whose tables failed to load in the background has no
			// children, so expanding it would look like nothing happened.
			// Surface the recorded error instead.
			if msg, failed := tree.loadErrorFor(nodeData.Database); failed {
				tree.Publish(models.StateChange{
					Key:   eventTreeError,
					Value: fmt.Sprintf("Could not open database %q:\n\n%s", nodeData.Database, msg),
				})
				return
			}
			tree.SetSelectedDatabase(nodeData.Database)
			node.SetExpanded(true)
		}
	case NodeTypeTable:
		tree.SetSelectedDatabase(nodeData.Database)
		if nodeData.Schema == "" {
			tree.SetSelectedTable(nodeData.Name)
		} else {
			tree.SetSelectedTable(fmt.Sprintf("%s.%s", nodeData.Schema, nodeData.Name))
		}
	case NodeTypeProcedure:
		tree.SetSelectedDatabase(nodeData.Database)
		if nodeData.Schema == "" {
			tree.SetSelectedProcedure(nodeData.Name)
		} else {
			tree.SetSelectedProcedure(fmt.Sprintf("%s.%s", nodeData.Schema, nodeData.Name))
		}
	case NodeTypeFunction:
		tree.SetSelectedDatabase(nodeData.Database)
		if nodeData.Schema == "" {
			tree.SetSelectedUserDefinedFunction(nodeData.Name)
		} else {
			tree.SetSelectedUserDefinedFunction(fmt.Sprintf("%s.%s", nodeData.Schema, nodeData.Name))
		}
	case NodeTypeView:
		tree.SetSelectedDatabase(nodeData.Database)
		if nodeData.Schema == "" {
			tree.SetSelectedView(nodeData.Name)
		} else {
			tree.SetSelectedView(fmt.Sprintf("%s.%s", nodeData.Schema, nodeData.Name))
		}
	default:
		break
	}
}

func (tree *Tree) databasesToNodes(children map[string][]string, node *tview.TreeNode, defaultExpanded bool) {
	node.ClearChildren()

	// Sort the keys and use them to loop over the
	// children so they are always in the same order.
	sortedKeys := slices.Sorted(maps.Keys(children))

	for _, key := range sortedKeys {
		// Filter schemas if Schemas is configured (PostgreSQL/MSSQL)
		if len(tree.Schemas) > 0 && tree.DBDriver.UseSchemas() {
			found := false
			for _, schema := range tree.Schemas {
				if schema == key {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		values := children[key]

		// Sort the values.
		sort.Strings(values)

		var tablesContainer *tview.TreeNode
		var rootNode *tview.TreeNode

		nodeReference := node.GetReference().(string)

		if key != nodeReference {
			rootNode = tview.NewTreeNode(key)
			rootNode.SetExpanded(false)
			rootNode.SetReference(key)
			rootNode.SetColor(app.Styles.PrimaryTextColor)
			node.AddChild(rootNode)
			tablesContainer = rootNode
		} else {
			tablesContainer = node
		}

		supportsProgramming := tree.DBDriver.SupportsProgramming()
		if supportsProgramming {
			tablesNode := tview.NewTreeNode("tables")
			tablesNode.SetExpanded(false)
			tablesNode.SetColor(app.Styles.PrimaryTextColor)

			if rootNode != nil {
				tablesNode.SetReference(fmt.Sprintf("%s.tables", key))
				rootNode.AddChild(tablesNode)
			} else {
				tablesNode.SetReference(fmt.Sprintf("%s.tables", nodeReference))
				node.AddChild(tablesNode)
			}

			tablesContainer = tablesNode
		}

		for _, child := range values {
			childNode := tview.NewTreeNode(child)
			childNode.SetExpanded(defaultExpanded)
			childNode.SetColor(app.Styles.PrimaryTextColor)

			if tree.DBDriver.UseSchemas() {
				if supportsProgramming {
					childNode.SetReference(fmt.Sprintf("%s.%s.tables.%s", nodeReference, key, child))
				} else {
					childNode.SetReference(fmt.Sprintf("%s.%s.%s", nodeReference, key, child))
				}
			} else {
				if supportsProgramming {
					childNode.SetReference(fmt.Sprintf("%s.tables.%s", key, child))
				} else {
					childNode.SetReference(fmt.Sprintf("%s.%s", key, child))
				}
			}

			tablesContainer.AddChild(childNode)
		}
	}
}

func (tree *Tree) addProgrammingNodes(functions map[string][]string, procedures map[string][]string, views map[string][]string, node *tview.TreeNode) {
	database := node.GetText()
	dbFunctions := functions[database]
	sort.Strings(dbFunctions)

	var functionsNode *tview.TreeNode
	functionsNodeReference := fmt.Sprintf("%s.functions", node.GetReference().(string))
	functionsNode = tview.NewTreeNode("functions")
	functionsNode.SetExpanded(false)
	functionsNode.SetReference(functionsNodeReference)
	functionsNode.SetColor(app.Styles.PrimaryTextColor)
	node.AddChild(functionsNode)

	for _, function := range dbFunctions {
		functionNode := tview.NewTreeNode(function)
		functionNode.SetExpanded(false)
		functionNode.SetColor(app.Styles.PrimaryTextColor)
		functionNode.SetReference(fmt.Sprintf("%s.%s", functionsNodeReference, function))
		functionsNode.AddChild(functionNode)
	}

	dbProcedures := procedures[database]
	sort.Strings(dbProcedures)

	var proceduresNode *tview.TreeNode
	proceduresNodeReference := fmt.Sprintf("%s.procedures", node.GetReference().(string))
	proceduresNode = tview.NewTreeNode("procedures")
	proceduresNode.SetExpanded(false)
	proceduresNode.SetReference(proceduresNodeReference)
	proceduresNode.SetColor(app.Styles.PrimaryTextColor)
	node.AddChild(proceduresNode)

	for _, procedure := range dbProcedures {
		procedureNode := tview.NewTreeNode(procedure)
		procedureNode.SetExpanded(false)
		procedureNode.SetColor(app.Styles.PrimaryTextColor)
		procedureNode.SetReference(fmt.Sprintf("%s.%s", proceduresNodeReference, procedure))
		proceduresNode.AddChild(procedureNode)
	}

	dbViews := views[database]
	sort.Strings(dbViews)

	var viewsNode *tview.TreeNode
	viewsNodeReference := fmt.Sprintf("%s.views", node.GetReference().(string))
	viewsNode = tview.NewTreeNode("views")
	viewsNode.SetExpanded(false)
	viewsNode.SetReference(viewsNodeReference)
	viewsNode.SetColor(app.Styles.PrimaryTextColor)
	node.AddChild(viewsNode)

	for _, view := range dbViews {
		viewNode := tview.NewTreeNode(view)
		viewNode.SetExpanded(false)
		viewNode.SetColor(app.Styles.PrimaryTextColor)
		viewNode.SetReference(fmt.Sprintf("%s.%s", viewsNodeReference, view))
		viewsNode.AddChild(viewNode)
	}
}

// stripColorTags removes tview color formatting like [black:primary] from node text
func stripColorTags(text string) string {
	for {
		start := strings.Index(text, "[")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "]")
		if end == -1 {
			break
		}
		end += start // make absolute

		inner := text[start+1 : end]
		// tview color tags never contain spaces: [black:primary], [red], [green:black:b]
		if !strings.Contains(inner, " ") {
			text = text[:start] + text[end+1:]
		} else {
			// Not a color tag: replace '[' with sentinel so we don't loop forever
			text = text[:start] + "\x00" + text[start+1:]
		}
	}
	return strings.ReplaceAll(text, "\x00", "[")
}

func prioritizeResult(pattern, target string, fuzzyRank int) int {
	// play match golf - lowest score wins

	// Exact match
	if pattern == target {
		return 0
	}

	// Prefix is scored on length difference, 1-99
	if strings.HasPrefix(target, pattern) {
		lengthDiff := len(target) - len(pattern)
		if lengthDiff > 98 {
			lengthDiff = 98
		}
		return 1 + lengthDiff
	}

	// Substr penalized by distance from start and length diff
	if strings.Contains(target, pattern) {
		index := strings.Index(target, pattern)
		lengthPenalty := len(target) - len(pattern)
		score := 100 + index + lengthPenalty
		if score > 9999 {
			score = 9999
		}
		return score
	}

	// If no other matches, fall back to fuzzy match with a low score
	return 10000 + fuzzyRank
}

// expandAncestors expands all ancestor nodes of the given node up to (but not including) root.
// tview TreeNode doesn't expose a parent pointer, so we walk from root to find the path.
func expandAncestors(target *tview.TreeNode, root *tview.TreeNode) {
	// Collect ancestors by walking the tree with a parent stack
	type stackEntry struct {
		node   *tview.TreeNode
		parent *tview.TreeNode
	}
	stack := []stackEntry{{node: root, parent: nil}}
	var ancestors []*tview.TreeNode

	for len(stack) > 0 {
		entry := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if entry.node == target {
			// Build ancestor chain walking back up
			for e := entry.parent; e != nil && e != root; {
				ancestors = append(ancestors, e)
				// Find e's parent by walking the tree (brute force but tree is small)
				found := false
				root.Walk(func(n, p *tview.TreeNode) bool {
					if n == e {
						e = p
						found = true
						return false
					}
					return true
				})
				if !found {
					break
				}
			}
			// Expand from top to bottom
			for i := len(ancestors) - 1; i >= 0; i-- {
				ancestors[i].SetExpanded(true)
			}
			return
		}

		for _, child := range entry.node.GetChildren() {
			stack = append(stack, stackEntry{node: child, parent: entry.node})
		}
	}
}

func (tree *Tree) search(searchText string) {
	rootNode := tree.GetRoot()
	lowerSearchText := strings.ToLower(searchText)
	tree.state.searchFoundNodes = []*tview.TreeNode{}

	// In flat mode the filter narrows the visible list to matching tables only,
	// rather than just highlighting matches within a fixed tree.
	if tree.flat {
		tree.filterFlat(lowerSearchText)
		return
	}

	if lowerSearchText == "" {
		rootNode.Walk(func(_, parent *tview.TreeNode) bool {
			if parent != nil && parent != rootNode && parent.IsExpanded() {
				parent.SetExpanded(false)
			}
			return true
		})
		return
	}

	parts := strings.SplitN(lowerSearchText, " ", 2)
	databaseNameFilter := ""
	tableNameFilter := ""

	if len(parts) == 1 {
		tableNameFilter = parts[0]
	} else {
		databaseNameFilter = parts[0]
		tableNameFilter = parts[1]
	}

	// Collect nodes with their match ranks
	type rankedNode struct {
		node *tview.TreeNode
		rank int
	}
	var rankedNodes []rankedNode

	rootNode.Walk(func(node, parent *tview.TreeNode) bool {
		nodeText := strings.ToLower(stripColorTags(node.GetText()))

		if databaseNameFilter == "" {
			rank := fuzzy.RankMatch(tableNameFilter, nodeText)
			if rank >= 0 {
				adjustedRank := prioritizeResult(tableNameFilter, nodeText, rank)
				rankedNodes = append(rankedNodes, rankedNode{node: node, rank: adjustedRank})
			}
		} else {
			rank := fuzzy.RankMatch(tableNameFilter, nodeText)
			if rank >= 0 && parent != nil {
				parentText := strings.ToLower(stripColorTags(parent.GetText()))
				parentRank := fuzzy.RankMatch(databaseNameFilter, parentText)
				if parentRank >= 0 {
					adjustedTableRank := prioritizeResult(tableNameFilter, nodeText, rank)
					adjustedParentRank := prioritizeResult(databaseNameFilter, parentText, parentRank)
					// Combine ranks: prioritize table match but factor in database match
					combinedRank := adjustedTableRank + (adjustedParentRank / 2)
					rankedNodes = append(rankedNodes, rankedNode{node: node, rank: combinedRank})
				}
			}
		}

		return true
	})

	sort.Slice(rankedNodes, func(i, j int) bool {
		return rankedNodes[i].rank < rankedNodes[j].rank
	})

	for _, rn := range rankedNodes {
		tree.state.searchFoundNodes = append(tree.state.searchFoundNodes, rn.node)
	}

	// Set current node to best match
	if len(tree.state.searchFoundNodes) > 0 {
		bestNode := tree.state.searchFoundNodes[0]
		expandAncestors(bestNode, rootNode)
		tree.SetCurrentNode(bestNode)
		tree.state.currentFocusFoundNode = bestNode
	}
}

// filterFlat rebuilds the flat table list so only tables matching the filter
// text remain visible, ranked best-match first. An empty filter restores the
// full list.
func (tree *Tree) filterFlat(lowerSearchText string) {
	rootNode := tree.GetRoot()

	if lowerSearchText == "" {
		// Restructure the tree's children and redraw on the main goroutine.
		// Mutating the children off-thread races the event loop's draw and
		// panics with a nil child. filterFlat is only ever reached from a
		// background goroutine (see the search callers), so QueueUpdateDraw
		// here cannot deadlock the main loop.
		App.QueueUpdateDraw(func() {
			rootNode.ClearChildren()
			for _, node := range tree.state.flatNodes {
				rootNode.AddChild(node)
			}
			if children := rootNode.GetChildren(); len(children) > 0 {
				tree.SetCurrentNode(children[0])
			}
		})
		return
	}

	type rankedNode struct {
		node *tview.TreeNode
		rank int
	}
	var rankedNodes []rankedNode

	for _, node := range tree.state.flatNodes {
		nodeText := strings.ToLower(stripColorTags(node.GetText()))
		rank := fuzzy.RankMatch(lowerSearchText, nodeText)
		if rank >= 0 {
			rankedNodes = append(rankedNodes, rankedNode{
				node: node,
				rank: prioritizeResult(lowerSearchText, nodeText, rank),
			})
		}
	}

	sort.Slice(rankedNodes, func(i, j int) bool {
		return rankedNodes[i].rank < rankedNodes[j].rank
	})

	// Restructure the children and redraw on the main goroutine (see above).
	App.QueueUpdateDraw(func() {
		rootNode.ClearChildren()
		for _, rn := range rankedNodes {
			rootNode.AddChild(rn.node)
			tree.state.searchFoundNodes = append(tree.state.searchFoundNodes, rn.node)
		}

		if len(tree.state.searchFoundNodes) > 0 {
			bestNode := tree.state.searchFoundNodes[0]
			tree.SetCurrentNode(bestNode)
			tree.state.currentFocusFoundNode = bestNode
		}
	})
}

// Subscribe to changes in the tree state
func (tree *Tree) Subscribe() chan models.StateChange {
	subscriber := make(chan models.StateChange)
	tree.subscribers = append(tree.subscribers, subscriber)
	return subscriber
}

// Publish subscribers of changes in the tree state
func (tree *Tree) Publish(change models.StateChange) {
	for _, subscriber := range tree.subscribers {
		subscriber <- change
	}
}

// Getters and Setters
func (tree *Tree) GetSelectedDatabase() string {
	return tree.state.selectedDatabase
}

func (tree *Tree) GetSelectedTable() string {
	return tree.state.selectedTable
}

func (tree *Tree) GetIsFiltering() bool {
	return tree.state.isFiltering
}

func (tree *Tree) SetSelectedDatabase(database string) {
	tree.state.selectedDatabase = database
	tree.Publish(models.StateChange{
		Key:   eventTreeSelectedDatabase,
		Value: database,
	})
}

func (tree *Tree) SetSelectedTable(table string) {
	tree.state.selectedTable = table
	tree.Publish(models.StateChange{
		Key:   eventTreeSelectedTable,
		Value: table,
	})
}

func (tree *Tree) SetSelectedUserDefinedFunction(name string) {
	tree.Publish(models.StateChange{
		Key:   eventTreeSelectedFunction,
		Value: name,
	})
}

func (tree *Tree) SetSelectedProcedure(name string) {
	tree.Publish(models.StateChange{
		Key:   eventTreeSelectedProcedure,
		Value: name,
	})
}

func (tree *Tree) SetSelectedView(name string) {
	tree.Publish(models.StateChange{
		Key:   eventTreeSelectedView,
		Value: name,
	})
}

func (tree *Tree) SetIsFiltering(isFiltering bool) {
	tree.state.isFiltering = isFiltering
	tree.Publish(models.StateChange{
		Key:   eventTreeIsFiltering,
		Value: isFiltering,
	})
}

// Blur func
func (tree *Tree) RemoveHighlight() {
	tree.SetBorderColor(app.Styles.InverseTextColor)
	tree.SetGraphicsColor(app.Styles.InverseTextColor)
	tree.SetTitleColor(app.Styles.InverseTextColor)
	// tree.GetRoot().SetColor(app.Styles.InverseTextColor)

	childrens := tree.GetRoot().GetChildren()

	var currentRef interface{}
	if currentNode := tree.GetCurrentNode(); currentNode != nil {
		currentRef = currentNode.GetReference()
	}

	for _, children := range childrens {
		currentColor := children.GetColor()

		childrenIsCurrentNode := children.GetReference() == currentRef

		if !childrenIsCurrentNode && currentColor == app.Styles.PrimaryTextColor {
			children.SetColor(app.Styles.InverseTextColor)
		}

		childrenOfChildren := children.GetChildren()

		for _, children := range childrenOfChildren {
			currentColor := children.GetColor()

			childrenIsCurrentNode := children.GetReference() == currentRef

			if !childrenIsCurrentNode && currentColor == app.Styles.PrimaryTextColor {
				children.SetColor(app.Styles.InverseTextColor)
			}

		}

	}
}

func (tree *Tree) ForceRemoveHighlight() {
	tree.SetBorderColor(app.Styles.InverseTextColor)
	tree.SetGraphicsColor(app.Styles.InverseTextColor)
	tree.SetTitleColor(app.Styles.InverseTextColor)
	tree.GetRoot().SetColor(app.Styles.InverseTextColor)

	childrens := tree.GetRoot().GetChildren()

	for _, children := range childrens {

		children.SetColor(app.Styles.InverseTextColor)

		childrenOfChildren := children.GetChildren()

		for _, children := range childrenOfChildren {
			children.SetColor(app.Styles.InverseTextColor)
		}

	}
}

// Focus func
func (tree *Tree) Highlight() {
	tree.SetBorderColor(app.Styles.PrimaryTextColor)
	tree.SetGraphicsColor(app.Styles.PrimaryTextColor)
	tree.SetTitleColor(app.Styles.PrimaryTextColor)
	tree.GetRoot().SetColor(app.Styles.PrimaryTextColor)

	childrens := tree.GetRoot().GetChildren()

	for _, children := range childrens {
		currentColor := children.GetColor()

		if currentColor == app.Styles.InverseTextColor {
			children.SetColor(app.Styles.PrimaryTextColor)

			childrenOfChildren := children.GetChildren()

			for _, children := range childrenOfChildren {
				currentColor := children.GetColor()

				if currentColor == app.Styles.InverseTextColor {
					children.SetColor(app.Styles.PrimaryTextColor)
				}
			}

		}

	}
}

func (tree *Tree) goToNextFoundNode() {
	for i, node := range tree.state.searchFoundNodes {
		if node == tree.state.currentFocusFoundNode {
			var newFocusNodeIndex int

			if i+1 < len(tree.state.searchFoundNodes) {
				newFocusNodeIndex = i + 1
			} else {
				newFocusNodeIndex = 0
			}

			newFocusNode := tree.state.searchFoundNodes[newFocusNodeIndex]
			tree.SetCurrentNode(newFocusNode)
			tree.state.currentFocusFoundNode = newFocusNode
			tree.FoundNodeCountInput.SetText(fmt.Sprintf("[%d/%d]", newFocusNodeIndex+1, len(tree.state.searchFoundNodes)))
			break
		}
	}
}

func (tree *Tree) goToPreviousFoundNode() {
	for i, node := range tree.state.searchFoundNodes {
		if node == tree.state.currentFocusFoundNode {
			var newFocusNodeIndex int

			if i-1 >= 0 {
				newFocusNodeIndex = i - 1
			} else {
				newFocusNodeIndex = len(tree.state.searchFoundNodes) - 1
			}

			newFocusNode := tree.state.searchFoundNodes[newFocusNodeIndex]
			tree.SetCurrentNode(newFocusNode)
			tree.state.currentFocusFoundNode = newFocusNode
			tree.FoundNodeCountInput.SetText(fmt.Sprintf("[%d/%d]", newFocusNodeIndex+1, len(tree.state.searchFoundNodes)))
			break
		}
	}
}

func (tree *Tree) CollapseAll() {
	tree.GetRoot().Walk(func(node, _ *tview.TreeNode) bool {
		if node.IsExpanded() && node != tree.GetRoot() {
			node.Collapse()
		}
		return true
	})
}

func (tree *Tree) ExpandAll() {
	tree.GetRoot().Walk(func(node, _ *tview.TreeNode) bool {
		if !node.IsExpanded() && node != tree.GetRoot() {
			node.Expand()
		}
		return true
	})
}

func (tree *Tree) InitializeNodes(dbName string) {
	rootNode := tree.GetRoot()
	if rootNode == nil {
		panic("Internal Error: No tree root")
	}

	// When the connection targets a specific database, skip the
	// database/schema accordion entirely and list its tables as a flat,
	// first-level list. The database/schema is shown as the pane header.
	if dbName != "" {
		tree.flatNodes(sanitizeDBName(dbName))
		return
	}

	dbs, err := tree.DBDriver.GetDatabases()
	if err != nil {
		panic(err.Error())
	}
	databases := make([]string, 0, len(dbs))
	for _, db := range dbs {
		databases = append(databases, sanitizeDBName(db))
	}

	for _, database := range databases {
		childNode := tview.NewTreeNode(database)
		childNode.SetExpanded(false)
		childNode.SetReference(database)
		childNode.SetColor(app.Styles.PrimaryTextColor)
		rootNode.AddChild(childNode)

		go func(database string, node *tview.TreeNode) {
			tables, err := tree.DBDriver.GetTables(database)
			if err != nil {
				tree.recordLoadError(database, node, err)
				return
			}

			var functions, procedures, views map[string][]string
			supportsProgramming := tree.DBDriver.SupportsProgramming()

			if supportsProgramming {
				functions, err = tree.DBDriver.GetFunctions(database)
				if err != nil {
					tree.recordLoadError(database, node, err)
					return
				}

				procedures, err = tree.DBDriver.GetProcedures(database)
				if err != nil {
					tree.recordLoadError(database, node, err)
					return
				}

				views, err = tree.DBDriver.GetViews(database)
				if err != nil {
					tree.recordLoadError(database, node, err)
					return
				}
			}

			// Mutate the tree and redraw on the main goroutine. tview's tree is
			// not safe to modify while the event loop is drawing it; doing the
			// AddChild off-thread races the draw and panics with a nil child.
			App.QueueUpdateDraw(func() {
				tree.databasesToNodes(tables, node, true)

				if supportsProgramming {
					tree.addProgrammingNodes(functions, procedures, views, node)
				}
			})
		}(database, childNode)
	}
}

// recordLoadError remembers why a database failed to load and tints its node so
// the failure is visible instead of presenting as a silently empty node. The
// message is surfaced when the user selects the database (see selectNode).
func (tree *Tree) recordLoadError(database string, node *tview.TreeNode, err error) {
	logger.Error(err.Error(), map[string]any{"database": database})

	tree.loadErrorsMu.Lock()
	tree.loadErrors[database] = err.Error()
	tree.loadErrorsMu.Unlock()

	App.QueueUpdateDraw(func() {
		node.SetColor(tcell.ColorRed)
	})
}

// loadErrorFor returns the recorded load error for a database, if any.
func (tree *Tree) loadErrorFor(database string) (string, bool) {
	tree.loadErrorsMu.Lock()
	defer tree.loadErrorsMu.Unlock()
	msg, ok := tree.loadErrors[database]
	return msg, ok
}

// flatNodes lists the tables of a single database as direct children of the
// root, with no database/schema parent nodes. References keep the same
// dot-notation format the accordion uses so selection behaves identically.
func (tree *Tree) flatNodes(database string) {
	rootNode := tree.GetRoot()

	go func() {
		tables, err := tree.DBDriver.GetTables(database)
		if err != nil {
			logger.Error(err.Error(), nil)
			return
		}

		useSchemas := tree.DBDriver.UseSchemas()
		supportsProgramming := tree.DBDriver.SupportsProgramming()

		// Collect the schema keys that survive the optional schema filter so we
		// know whether to disambiguate table labels with a schema prefix.
		schemaKeys := make([]string, 0, len(tables))
		for _, key := range slices.Sorted(maps.Keys(tables)) {
			if len(tree.Schemas) > 0 && useSchemas && !slices.Contains(tree.Schemas, key) {
				continue
			}
			schemaKeys = append(schemaKeys, key)
		}
		multipleSchemas := len(schemaKeys) > 1

		// Build the nodes off-thread; they aren't attached to the live tree yet.
		nodes := make([]*tview.TreeNode, 0)
		for _, key := range schemaKeys {
			values := tables[key]
			sort.Strings(values)

			for _, table := range values {
				var reference, label string
				switch {
				case useSchemas && supportsProgramming:
					reference = fmt.Sprintf("%s.%s.tables.%s", database, key, table)
				case useSchemas:
					reference = fmt.Sprintf("%s.%s.%s", database, key, table)
				case supportsProgramming:
					reference = fmt.Sprintf("%s.tables.%s", key, table)
				default:
					reference = fmt.Sprintf("%s.%s", key, table)
				}

				if useSchemas && multipleSchemas {
					label = fmt.Sprintf("%s.%s", key, table)
				} else {
					label = table
				}

				node := tview.NewTreeNode(label)
				node.SetReference(reference)
				node.SetColor(app.Styles.PrimaryTextColor)
				nodes = append(nodes, node)
			}
		}

		// SetSelectedDatabase publishes to subscribers over a blocking channel,
		// so it must stay off the main goroutine or it deadlocks the event loop.
		tree.SetSelectedDatabase(database)

		// Attach the nodes to the live tree and redraw on the main goroutine.
		// tview's tree is not safe to mutate while the event loop is drawing it;
		// doing the AddChild off-thread races the draw and panics with a nil
		// child.
		App.QueueUpdateDraw(func() {
			// Reset the master list so a Refresh doesn't accumulate duplicates.
			tree.state.flatNodes = nodes
			for _, node := range nodes {
				rootNode.AddChild(node)
			}

			// Root is hidden in flat mode, so point the cursor at the first table
			// for keyboard navigation once the user leaves search mode.
			if children := rootNode.GetChildren(); len(children) > 0 {
				tree.SetCurrentNode(children[0])
			}
		})
	}()
}

func (tree *Tree) Refresh(dbName string) {
	rootNode := tree.GetRoot()
	rootNode.ClearChildren()
	// re-add nodes
	tree.InitializeNodes(dbName)
}

func (tree *Tree) ClearSearch() {
	// search() must run off the main goroutine: in flat mode it redraws via a
	// queued update, which deadlocks if invoked from a main-goroutine handler.
	go tree.search("")
	tree.FoundNodeCountInput.SetText("")
	tree.SetBorderPadding(0, 0, 0, 0)
	tree.Filter.SetText("")
}

func sanitizeDBName(dbName string) string {
	// Remove dots from db name
	return strings.ReplaceAll(dbName, ".", "_")
}
