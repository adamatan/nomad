package zclsyntax

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-zcl/zcl"
)

func (p *parser) ParseTemplate() (Expression, zcl.Diagnostics) {
	return p.parseTemplate(TokenEOF)
}

func (p *parser) parseTemplate(end TokenType) (Expression, zcl.Diagnostics) {
	exprs, passthru, rng, diags := p.parseTemplateInner(end)

	if passthru {
		if len(exprs) != 1 {
			panic("passthru set with len(exprs) != 1")
		}
		return &TemplateWrapExpr{
			Wrapped:  exprs[0],
			SrcRange: rng,
		}, diags
	}

	return &TemplateExpr{
		Parts:    exprs,
		SrcRange: rng,
	}, diags
}

func (p *parser) parseTemplateInner(end TokenType) ([]Expression, bool, zcl.Range, zcl.Diagnostics) {
	parts, diags := p.parseTemplateParts(end)
	tp := templateParser{
		Tokens:   parts.Tokens,
		SrcRange: parts.SrcRange,
	}
	exprs, exprsDiags := tp.parseRoot()
	diags = append(diags, exprsDiags...)

	passthru := false
	if len(parts.Tokens) == 2 { // one real token and one synthetic "end" token
		if _, isInterp := parts.Tokens[0].(*templateInterpToken); isInterp {
			passthru = true
		}
	}

	return exprs, passthru, parts.SrcRange, diags
}

type templateParser struct {
	Tokens   []templateToken
	SrcRange zcl.Range

	pos int
}

func (p *templateParser) parseRoot() ([]Expression, zcl.Diagnostics) {
	var exprs []Expression
	var diags zcl.Diagnostics

	for {
		next := p.Peek()
		if _, isEnd := next.(*templateEndToken); isEnd {
			break
		}

		expr, exprDiags := p.parseExpr()
		diags = append(diags, exprDiags...)
		exprs = append(exprs, expr)
	}

	return exprs, diags
}

func (p *templateParser) parseExpr() (Expression, zcl.Diagnostics) {
	next := p.Peek()
	switch tok := next.(type) {

	case *templateLiteralToken:
		p.Read() // eat literal
		return &LiteralValueExpr{
			Val:      cty.StringVal(tok.Val),
			SrcRange: tok.SrcRange,
		}, nil

	case *templateInterpToken:
		p.Read() // eat interp
		return tok.Expr, nil

	case *templateIfToken:
		return p.parseIf()

	case *templateForToken:
		return p.parseFor()

	case *templateEndToken:
		p.Read() // eat erroneous token
		return errPlaceholderExpr(tok.SrcRange), zcl.Diagnostics{
			{
				// This is a particularly unhelpful diagnostic, so callers
				// should attempt to pre-empt it and produce a more helpful
				// diagnostic that is context-aware.
				Severity: zcl.DiagError,
				Summary:  "Unexpected end of template",
				Detail:   "The control directives within this template are unbalanced.",
				Subject:  &tok.SrcRange,
			},
		}

	case *templateEndCtrlToken:
		p.Read() // eat erroneous token
		return errPlaceholderExpr(tok.SrcRange), zcl.Diagnostics{
			{
				Severity: zcl.DiagError,
				Summary:  fmt.Sprintf("Unexpected %s directive", tok.Name()),
				Detail:   "The control directives within this template are unbalanced.",
				Subject:  &tok.SrcRange,
			},
		}

	default:
		// should never happen, because above should be exhaustive
		panic(fmt.Sprintf("unhandled template token type %T", next))
	}
}

func (p *templateParser) parseIf() (Expression, zcl.Diagnostics) {
	open := p.Read()
	openIf, isIf := open.(*templateIfToken)
	if !isIf {
		// should never happen if caller is behaving
		panic("parseIf called with peeker not pointing at if token")
	}

	var ifExprs, elseExprs []Expression
	var diags zcl.Diagnostics
	var endifRange zcl.Range

	currentExprs := &ifExprs
Token:
	for {
		next := p.Peek()
		if end, isEnd := next.(*templateEndToken); isEnd {
			diags = append(diags, &zcl.Diagnostic{
				Severity: zcl.DiagError,
				Summary:  "Unexpected end of template",
				Detail: fmt.Sprintf(
					"The if directive at %s is missing its corresponding endif directive.",
					openIf.SrcRange,
				),
				Subject: &end.SrcRange,
			})
			return errPlaceholderExpr(end.SrcRange), diags
		}
		if end, isCtrlEnd := next.(*templateEndCtrlToken); isCtrlEnd {
			p.Read() // eat end directive

			switch end.Type {

			case templateElse:
				if currentExprs == &ifExprs {
					currentExprs = &elseExprs
					continue Token
				}

				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  "Unexpected else directive",
					Detail: fmt.Sprintf(
						"Already in the else clause for the if started at %s.",
						openIf.SrcRange,
					),
					Subject: &end.SrcRange,
				})

			case templateEndIf:
				endifRange = end.SrcRange
				break Token

			default:
				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  fmt.Sprintf("Unexpected %s directive", end.Name()),
					Detail: fmt.Sprintf(
						"Expecting an endif directive for the if started at %s.",
						openIf.SrcRange,
					),
					Subject: &end.SrcRange,
				})
			}

			return errPlaceholderExpr(end.SrcRange), diags
		}

		expr, exprDiags := p.parseExpr()
		diags = append(diags, exprDiags...)
		*currentExprs = append(*currentExprs, expr)
	}

	if len(ifExprs) == 0 {
		ifExprs = append(ifExprs, &LiteralValueExpr{
			Val: cty.StringVal(""),
			SrcRange: zcl.Range{
				Filename: openIf.SrcRange.Filename,
				Start:    openIf.SrcRange.End,
				End:      openIf.SrcRange.End,
			},
		})
	}
	if len(elseExprs) == 0 {
		elseExprs = append(elseExprs, &LiteralValueExpr{
			Val: cty.StringVal(""),
			SrcRange: zcl.Range{
				Filename: endifRange.Filename,
				Start:    endifRange.Start,
				End:      endifRange.Start,
			},
		})
	}

	trueExpr := &TemplateExpr{
		Parts:    ifExprs,
		SrcRange: zcl.RangeBetween(ifExprs[0].Range(), ifExprs[len(ifExprs)-1].Range()),
	}
	falseExpr := &TemplateExpr{
		Parts:    elseExprs,
		SrcRange: zcl.RangeBetween(elseExprs[0].Range(), elseExprs[len(elseExprs)-1].Range()),
	}

	return &ConditionalExpr{
		Condition:   openIf.CondExpr,
		TrueResult:  trueExpr,
		FalseResult: falseExpr,

		SrcRange: zcl.RangeBetween(openIf.SrcRange, endifRange),
	}, diags
}

func (p *templateParser) parseFor() (Expression, zcl.Diagnostics) {
	open := p.Read()
	openFor, isFor := open.(*templateForToken)
	if !isFor {
		// should never happen if caller is behaving
		panic("parseFor called with peeker not pointing at for token")
	}

	var contentExprs []Expression
	var diags zcl.Diagnostics
	var endforRange zcl.Range

Token:
	for {
		next := p.Peek()
		if end, isEnd := next.(*templateEndToken); isEnd {
			diags = append(diags, &zcl.Diagnostic{
				Severity: zcl.DiagError,
				Summary:  "Unexpected end of template",
				Detail: fmt.Sprintf(
					"The for directive at %s is missing its corresponding endfor directive.",
					openFor.SrcRange,
				),
				Subject: &end.SrcRange,
			})
			return errPlaceholderExpr(end.SrcRange), diags
		}
		if end, isCtrlEnd := next.(*templateEndCtrlToken); isCtrlEnd {
			p.Read() // eat end directive

			switch end.Type {

			case templateElse:
				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  "Unexpected else directive",
					Detail:   "An else clause is not expected for a for directive.",
					Subject:  &end.SrcRange,
				})

			case templateEndFor:
				endforRange = end.SrcRange
				break Token

			default:
				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  fmt.Sprintf("Unexpected %s directive", end.Name()),
					Detail: fmt.Sprintf(
						"Expecting an endfor directive corresponding to the for directive at %s.",
						openFor.SrcRange,
					),
					Subject: &end.SrcRange,
				})
			}

			return errPlaceholderExpr(end.SrcRange), diags
		}

		expr, exprDiags := p.parseExpr()
		diags = append(diags, exprDiags...)
		contentExprs = append(contentExprs, expr)
	}

	if len(contentExprs) == 0 {
		contentExprs = append(contentExprs, &LiteralValueExpr{
			Val: cty.StringVal(""),
			SrcRange: zcl.Range{
				Filename: openFor.SrcRange.Filename,
				Start:    openFor.SrcRange.End,
				End:      openFor.SrcRange.End,
			},
		})
	}

	contentExpr := &TemplateExpr{
		Parts:    contentExprs,
		SrcRange: zcl.RangeBetween(contentExprs[0].Range(), contentExprs[len(contentExprs)-1].Range()),
	}

	forExpr := &ForExpr{
		KeyVar: openFor.KeyVar,
		ValVar: openFor.ValVar,

		CollExpr: openFor.CollExpr,
		ValExpr:  contentExpr,

		SrcRange:   zcl.RangeBetween(openFor.SrcRange, endforRange),
		OpenRange:  openFor.SrcRange,
		CloseRange: endforRange,
	}

	return &TemplateJoinExpr{
		Tuple: forExpr,
	}, diags
}

func (p *templateParser) Peek() templateToken {
	return p.Tokens[p.pos]
}

func (p *templateParser) Read() templateToken {
	ret := p.Peek()
	if _, end := ret.(*templateEndToken); !end {
		p.pos++
	}
	return ret
}

// parseTemplateParts produces a flat sequence of "template tokens", which are
// either literal values (with any "trimming" already applied), interpolation
// sequences, or control flow markers.
//
// A further pass is required on the result to turn it into an AST.
func (p *parser) parseTemplateParts(end TokenType) (*templateParts, zcl.Diagnostics) {
	var parts []templateToken
	var diags zcl.Diagnostics

	startRange := p.NextRange()
	ltrimNext := false
	nextCanTrimPrev := false
	var endRange zcl.Range

Token:
	for {
		next := p.Read()
		if next.Type == end {
			// all done!
			endRange = next.Range
			break
		}

		ltrim := ltrimNext
		ltrimNext = false
		canTrimPrev := nextCanTrimPrev
		nextCanTrimPrev = false

		switch next.Type {
		case TokenStringLit, TokenQuotedLit:
			str, strDiags := p.decodeStringLit(next)
			diags = append(diags, strDiags...)

			if ltrim {
				str = strings.TrimLeftFunc(str, unicode.IsSpace)
			}

			parts = append(parts, &templateLiteralToken{
				Val:      str,
				SrcRange: next.Range,
			})
			nextCanTrimPrev = true

		case TokenTemplateInterp:
			// if the opener is ${~ then we want to eat any trailing whitespace
			// in the preceding literal token, assuming it is indeed a literal
			// token.
			if canTrimPrev && len(next.Bytes) == 3 && next.Bytes[2] == '~' && len(parts) > 0 {
				prevExpr := parts[len(parts)-1]
				if lexpr, ok := prevExpr.(*templateLiteralToken); ok {
					lexpr.Val = strings.TrimRightFunc(lexpr.Val, unicode.IsSpace)
				}
			}

			p.PushIncludeNewlines(false)
			expr, exprDiags := p.ParseExpression()
			diags = append(diags, exprDiags...)
			close := p.Peek()
			if close.Type != TokenTemplateSeqEnd {
				if !p.recovery {
					diags = append(diags, &zcl.Diagnostic{
						Severity: zcl.DiagError,
						Summary:  "Extra characters after interpolation expression",
						Detail:   "Expected a closing brace to end the interpolation expression, but found extra characters.",
						Subject:  &close.Range,
						Context:  zcl.RangeBetween(startRange, close.Range).Ptr(),
					})
				}
				p.recover(TokenTemplateSeqEnd)
			} else {
				p.Read() // eat closing brace

				// If the closer is ~} then we want to eat any leading
				// whitespace on the next token, if it turns out to be a
				// literal token.
				if len(close.Bytes) == 2 && close.Bytes[0] == '~' {
					ltrimNext = true
				}
			}
			p.PopIncludeNewlines()
			parts = append(parts, &templateInterpToken{
				Expr:     expr,
				SrcRange: zcl.RangeBetween(next.Range, close.Range),
			})

		case TokenTemplateControl:
			// if the opener is !{~ then we want to eat any trailing whitespace
			// in the preceding literal token, assuming it is indeed a literal
			// token.
			if canTrimPrev && len(next.Bytes) == 3 && next.Bytes[2] == '~' && len(parts) > 0 {
				prevExpr := parts[len(parts)-1]
				if lexpr, ok := prevExpr.(*templateLiteralToken); ok {
					lexpr.Val = strings.TrimRightFunc(lexpr.Val, unicode.IsSpace)
				}
			}
			p.PushIncludeNewlines(false)

			kw := p.Peek()
			if kw.Type != TokenIdent {
				if !p.recovery {
					diags = append(diags, &zcl.Diagnostic{
						Severity: zcl.DiagError,
						Summary:  "Invalid template directive",
						Detail:   "A template directive keyword (\"if\", \"for\", etc) is expected at the beginning of a !{ sequence.",
						Subject:  &kw.Range,
						Context:  zcl.RangeBetween(next.Range, kw.Range).Ptr(),
					})
				}
				p.recover(TokenTemplateSeqEnd)
				p.PopIncludeNewlines()
				continue Token
			}
			p.Read() // eat keyword token

			switch {

			case ifKeyword.TokenMatches(kw):
				condExpr, exprDiags := p.ParseExpression()
				diags = append(diags, exprDiags...)
				parts = append(parts, &templateIfToken{
					CondExpr: condExpr,
					SrcRange: zcl.RangeBetween(next.Range, p.NextRange()),
				})

			case elseKeyword.TokenMatches(kw):
				parts = append(parts, &templateEndCtrlToken{
					Type:     templateElse,
					SrcRange: zcl.RangeBetween(next.Range, p.NextRange()),
				})

			case endifKeyword.TokenMatches(kw):
				parts = append(parts, &templateEndCtrlToken{
					Type:     templateEndIf,
					SrcRange: zcl.RangeBetween(next.Range, p.NextRange()),
				})

			case forKeyword.TokenMatches(kw):
				var keyName, valName string
				if p.Peek().Type != TokenIdent {
					if !p.recovery {
						diags = append(diags, &zcl.Diagnostic{
							Severity: zcl.DiagError,
							Summary:  "Invalid 'for' directive",
							Detail:   "For directive requires variable name after 'for'.",
							Subject:  p.Peek().Range.Ptr(),
						})
					}
					p.recover(TokenTemplateSeqEnd)
					p.PopIncludeNewlines()
					continue Token
				}

				valName = string(p.Read().Bytes)

				if p.Peek().Type == TokenComma {
					// What we just read was actually the key, then.
					keyName = valName
					p.Read() // eat comma

					if p.Peek().Type != TokenIdent {
						if !p.recovery {
							diags = append(diags, &zcl.Diagnostic{
								Severity: zcl.DiagError,
								Summary:  "Invalid 'for' directive",
								Detail:   "For directive requires value variable name after comma.",
								Subject:  p.Peek().Range.Ptr(),
							})
						}
						p.recover(TokenTemplateSeqEnd)
						p.PopIncludeNewlines()
						continue Token
					}

					valName = string(p.Read().Bytes)
				}

				if !inKeyword.TokenMatches(p.Peek()) {
					if !p.recovery {
						diags = append(diags, &zcl.Diagnostic{
							Severity: zcl.DiagError,
							Summary:  "Invalid 'for' directive",
							Detail:   "For directive requires 'in' keyword after names.",
							Subject:  p.Peek().Range.Ptr(),
						})
					}
					p.recover(TokenTemplateSeqEnd)
					p.PopIncludeNewlines()
					continue Token
				}
				p.Read() // eat 'in' keyword

				collExpr, collDiags := p.ParseExpression()
				diags = append(diags, collDiags...)
				parts = append(parts, &templateForToken{
					KeyVar:   keyName,
					ValVar:   valName,
					CollExpr: collExpr,

					SrcRange: zcl.RangeBetween(next.Range, p.NextRange()),
				})

			case endforKeyword.TokenMatches(kw):
				parts = append(parts, &templateEndCtrlToken{
					Type:     templateEndFor,
					SrcRange: zcl.RangeBetween(next.Range, p.NextRange()),
				})

			default:
				if !p.recovery {
					suggestions := []string{"if", "for", "else", "endif", "endfor"}
					given := string(kw.Bytes)
					suggestion := nameSuggestion(given, suggestions)
					if suggestion != "" {
						suggestion = fmt.Sprintf(" Did you mean %q?", suggestion)
					}

					diags = append(diags, &zcl.Diagnostic{
						Severity: zcl.DiagError,
						Summary:  "Invalid template control keyword",
						Detail:   fmt.Sprintf("%q is not a valid template control keyword.%s", given, suggestion),
						Subject:  &kw.Range,
						Context:  zcl.RangeBetween(next.Range, kw.Range).Ptr(),
					})
				}
				p.recover(TokenTemplateSeqEnd)
				p.PopIncludeNewlines()
				continue Token

			}

			close := p.Peek()
			if close.Type != TokenTemplateSeqEnd {
				if !p.recovery {
					diags = append(diags, &zcl.Diagnostic{
						Severity: zcl.DiagError,
						Summary:  fmt.Sprintf("Extra characters in %s marker", kw.Bytes),
						Detail:   "Expected a closing brace to end the sequence, but found extra characters.",
						Subject:  &close.Range,
						Context:  zcl.RangeBetween(startRange, close.Range).Ptr(),
					})
				}
				p.recover(TokenTemplateSeqEnd)
			} else {
				p.Read() // eat closing brace

				// If the closer is ~} then we want to eat any leading
				// whitespace on the next token, if it turns out to be a
				// literal token.
				if len(close.Bytes) == 2 && close.Bytes[0] == '~' {
					ltrimNext = true
				}
			}
			p.PopIncludeNewlines()

		default:
			if !p.recovery {
				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  "Unterminated template string",
					Detail:   "No closing marker was found for the string.",
					Subject:  &next.Range,
					Context:  zcl.RangeBetween(startRange, next.Range).Ptr(),
				})
			}
			final := p.recover(end)
			endRange = final.Range
			break Token
		}
	}

	if len(parts) == 0 {
		// If a sequence has no content, we'll treat it as if it had an
		// empty string in it because that's what the user probably means
		// if they write "" in configuration.
		parts = append(parts, &templateLiteralToken{
			Val: "",
			SrcRange: zcl.Range{
				// Range is the zero-character span immediately after the
				// opening quote.
				Filename: startRange.Filename,
				Start:    startRange.End,
				End:      startRange.End,
			},
		})
	}

	// Always end with an end token, so the parser can produce diagnostics
	// about unclosed items with proper position information.
	parts = append(parts, &templateEndToken{
		SrcRange: endRange,
	})

	ret := &templateParts{
		Tokens:   parts,
		SrcRange: zcl.RangeBetween(startRange, endRange),
	}

	return ret, diags
}

type templateParts struct {
	Tokens   []templateToken
	SrcRange zcl.Range
}

// templateToken is a higher-level token that represents a single atom within
// the template language. Our template parsing first raises the raw token
// stream to a sequence of templateToken, and then transforms the result into
// an expression tree.
type templateToken interface {
	templateToken() templateToken
}

type templateLiteralToken struct {
	Val      string
	SrcRange zcl.Range
	isTemplateToken
}

type templateInterpToken struct {
	Expr     Expression
	SrcRange zcl.Range
	isTemplateToken
}

type templateIfToken struct {
	CondExpr Expression
	SrcRange zcl.Range
	isTemplateToken
}

type templateForToken struct {
	KeyVar   string // empty if ignoring key
	ValVar   string
	CollExpr Expression
	SrcRange zcl.Range
	isTemplateToken
}

type templateEndCtrlType int

const (
	templateEndIf templateEndCtrlType = iota
	templateElse
	templateEndFor
)

type templateEndCtrlToken struct {
	Type     templateEndCtrlType
	SrcRange zcl.Range
	isTemplateToken
}

func (t *templateEndCtrlToken) Name() string {
	switch t.Type {
	case templateEndIf:
		return "endif"
	case templateElse:
		return "else"
	case templateEndFor:
		return "endfor"
	default:
		// should never happen
		panic("invalid templateEndCtrlType")
	}
}

type templateEndToken struct {
	SrcRange zcl.Range
	isTemplateToken
}

type isTemplateToken [0]int

func (t isTemplateToken) templateToken() templateToken {
	return t
}