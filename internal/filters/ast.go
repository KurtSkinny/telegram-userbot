// ast.go содержит структуры и функции для построения и оценки
// абстрактного синтаксического дерева (AST) для фильтров сообщений.
package filters

import "telegram-userbot/internal/logger"

// evalNode вычисляет результат узла AST с учетом нормализованного текста
func evalNode(node *Node, text string) (bool, *Node) {
	switch node.Op {
	case "AND":
		return evalAnd(node, text)
	case "OR":
		return evalOr(node, text)
	case "NOT":
		return evalNot(node, text)
	case "AT_LEAST":
		return evalAtLeast(node, text)
	default:
		// Предполагаем, что это листовой узел
		return evalLeaf(node, text), node
	}
}

// evalAnd вычисляет AND операцию: все аргументы должны быть true
func evalAnd(node *Node, text string) (bool, *Node) {
	for _, arg := range node.Args {
		if match, matchedNode := evalNode(&arg, text); !match {
			return false, matchedNode
		}
	}
	return true, node
}

// evalOr вычисляет OR операцию: хотя бы один аргумент должен быть true
func evalOr(node *Node, text string) (bool, *Node) {
	for _, arg := range node.Args {
		if match, matchedNode := evalNode(&arg, text); match {
			return true, matchedNode
		}
	}
	return false, node
}

// evalNot вычисляет NOT операцию: ровно один аргумент должен быть false
func evalNot(node *Node, text string) (bool, *Node) {
	if match, matchedNode := evalNode(&node.Args[0], text); !match {
		return true, matchedNode
	}
	return false, node
}

// evalAtLeast вычисляет AT_LEAST операцию: минимум n аргументов должны быть true
func evalAtLeast(node *Node, text string) (bool, *Node) {
	matchedCount := 0
	var lastMatchedNode *Node

	for _, arg := range node.Args {
		if match, matchedNode := evalNode(&arg, text); match {
			matchedCount++
			lastMatchedNode = matchedNode
		}
	}

	if matchedCount >= node.N {
		return true, lastMatchedNode
	}
	return false, node
}

// evalLeaf вычисляет листовой узел используя предкомпилированный паттерн
func evalLeaf(node *Node, text string) bool {
	if node.CompiledPattern == nil {
		logger.Errorf("CompiledPattern is nil for node type=%s, value=%s, pattern=%s",
			node.Type, node.Value, node.Pattern)
		return false
	}

	matched := node.CompiledPattern.MatchString(text)

	if logger.IsDebugEnabled() {
		if matched {
			logger.Debugf("Pattern matched: type=%s, original=%s", node.Type,
				getOriginalValue(node))
		}
	}

	return matched
}

// getOriginalValue возвращает оригинальное значение для логирования
func getOriginalValue(node *Node) string {
	if node.Type == "kw" {
		return node.Value
	}
	return node.Pattern
}
